-- Phase 1 schema: the console's business state moves out of OSS objects and
-- into tables. The agent protocol does not change -- the Worker still writes
-- agent_workdir/policy.json, _bindings/<machine> and <user>/credentials.zip --
-- so every table here has to be able to reproduce those objects exactly.
--
-- Conventions used throughout:
--   * ids are uuid; nothing is keyed by a name a human can change.
--   * money is numeric, never float (spec 4.2).
--   * times are timestamptz and stored in UTC.
--   * "epoch" is a per-employee generation counter. It goes up whenever the
--     employee's credentials must stop working (re-issue, offboard). Tasks
--     carry the epoch they were created for, so a task that comes back from a
--     retry after the state moved on can be discarded instead of re-applying
--     an old token (spec 6.2).
--   * updated_at is set by the writing statement, not a trigger: a trigger
--     would also fire for the Worker's bookkeeping writes and make "when did
--     an administrator last change this" unanswerable.

create extension if not exists pgcrypto;

-- ---------------------------------------------------------------- employees

-- One row per person on the roster (admin/users.json today).
--
-- windows_user is unique and lower-cased. Windows account names are
-- case-insensitive, and the roster's Find() already compares that way; making
-- the database agree means "Work1" and "work1" cannot become two employees
-- with two sets of credentials in two directories.
--
-- It is NOT the identity: id is. An offboarded row keeps its id and its
-- history, and reopening the account flips status back rather than creating a
-- second row -- which is why the unique constraint spans both statuses.
create table employees (
  id             uuid primary key default gen_random_uuid(),
  windows_user   text not null unique check (windows_user = lower(windows_user) and windows_user <> ''),
  -- external_id is the HR employee number when there is one. Null, not '',
  -- for "unknown": several employees may be unknown, only one may hold a
  -- given number.
  external_id    text unique,
  name           text not null default '',
  department     text not null default '',
  -- Administrator notes: which login this person uses for each tool. Nothing
  -- authenticates with them (model.UserEntry).
  codex_account  text not null default '',
  claude_account text not null default '',
  status         text not null default 'active' check (status in ('active', 'offboarded')),
  auth_epoch     integer not null default 1 check (auth_epoch > 0),
  -- version is the optimistic-locking counter for administrator edits. A save
  -- that carries a stale version is rejected instead of overwriting whatever
  -- somebody else just wrote.
  version        integer not null default 1 check (version > 0),
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now(),
  offboarded_at  timestamptz,
  constraint employees_offboarded_at_matches_status
    check ((status = 'offboarded') = (offboarded_at is not null))
);

-- Which models this employee may call. Kept as rows rather than an array so a
-- single model can be revoked, and granted, by name.
create table employee_models (
  employee_id uuid not null references employees(id) on delete cascade,
  model       text not null check (model <> ''),
  granted_at  timestamptz not null default now(),
  primary key (employee_id, model)
);

-- The limits the console wants the gateway to enforce. One current row per
-- employee; previous values are recoverable from audit_events.before, which
-- is the only place a "who changed it" answer exists anyway.
--
-- The gateway holds the amount actually spent. It is deliberately not copied
-- here: two numbers that drift is worse than one number that is a round trip
-- away (2026-09-18 notes, section 3).
create table employee_quotas (
  employee_id    uuid primary key references employees(id) on delete cascade,
  monthly_budget numeric(12, 6) not null check (monthly_budget > 0),
  currency       text not null default 'USD' check (currency <> ''),
  rpm            integer not null check (rpm > 0),
  tpm            integer not null check (tpm > 0),
  parallel       integer not null check (parallel > 0),
  -- How the budget period rolls over. Verified against the live gateway on
  -- 2026-09-18: LiteLLM's "1mo" resets at 00:00 UTC on the 1st, regardless of
  -- when the account was opened -- not "signup + one month". period_tz is for
  -- display only; every boundary is computed in UTC.
  period_rule    text not null default 'calendar_month_utc'
                 check (period_rule in ('calendar_month_utc')),
  period_tz      text not null default 'Asia/Shanghai',
  version        integer not null default 1 check (version > 0),
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now()
);

-- ------------------------------------------------------------------ devices

-- One row per machine. Phase 1 still recognises an agent by hostname, because
-- that is all the agent sends; status records how far a machine has moved
-- toward the phase 2 protocol so both can run at once.
--
-- A renamed host is a new row and that is intentional: with no device key
-- there is nothing else to go on, and silently inheriting another machine's
-- binding is the worse failure.
create table devices (
  id            uuid primary key default gen_random_uuid(),
  hostname      text not null unique check (hostname <> ''),
  status        text not null default 'legacy_oss'
                check (status in ('legacy_oss', 'api_v1', 'revoked')),
  agent_version text not null default '',
  last_seen_at  timestamptz,
  note          text not null default '',
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  revoked_at    timestamptz,
  constraint devices_revoked_at_matches_status
    check ((status = 'revoked') = (revoked_at is not null))
);

-- Which employee a machine serves, over time. Unbinding sets unbound_at
-- instead of deleting, so "who had this machine in August" stays answerable.
--
-- The partial unique index is the real rule: at most one open binding per
-- device. Two open bindings would mean two credential sets racing into the
-- same machine.
create table device_bindings (
  id            uuid primary key default gen_random_uuid(),
  device_id     uuid not null references devices(id) on delete cascade,
  employee_id   uuid not null references employees(id),
  -- epoch counts bindings on this device, starting at 1. It appears in the
  -- exported binding object so an agent can tell a re-bind from a re-read.
  epoch         integer not null check (epoch > 0),
  note          text not null default '',
  bound_at      timestamptz not null default now(),
  bound_by      text not null default '',
  unbound_at    timestamptz,
  unbound_by    text not null default '',
  -- One-shot "end this user's Codex" request (model.Binding.RestartCodex).
  -- It rides on the binding because the binding is the one object every agent
  -- already reads every cycle. The agent echoes the nonce it acted on into
  -- its status; equal means done, different means still pending.
  restart_nonce text not null default '',
  restart_at    timestamptz,
  unique (device_id, epoch)
);
create unique index device_bindings_one_open_per_device
  on device_bindings (device_id) where unbound_at is null;
create index device_bindings_by_employee on device_bindings (employee_id);

-- What the machine says about itself: the newest _status/<machine> object,
-- imported by the Worker. It is a cache of the agent's report, never a source
-- of truth about what the machine is supposed to have.
--
-- The columns are the ones the console lists and filters on; report keeps the
-- whole object so a field added to model.Status is visible before any
-- migration, and source_etag lets an unchanged object be skipped.
create table device_reported_state (
  device_id           uuid primary key references devices(id) on delete cascade,
  imported_at         timestamptz not null default now(),
  -- Status.LastSync, as reported. Imports with an older value than the row
  -- already holds are dropped: object stores do not promise read ordering,
  -- and a stale status overwriting a fresh one reads as a machine going dark.
  last_sync_at        timestamptz,
  agent_version       text not null default '',
  bound_windows_user  text not null default '',
  bound_user_exists   boolean,
  policy_etag         text not null default '',
  creds_etag          text not null default '',
  creds_applied       boolean,
  applocker_mode      text not null default '',
  collect_enabled     boolean,
  collect_uploaded    integer not null default 0,
  codex_version       text not null default '',
  codex_state         text not null default '',
  codex_restart_nonce text not null default '',
  codex_restart_at    timestamptz,
  codex_restart_note  text not null default '',
  last_event          text not null default '',
  last_event_at       timestamptz,
  error_count         integer not null default 0,
  warning_count       integer not null default 0,
  report              jsonb not null,
  source_etag         text not null default ''
);

-- ----------------------------------------------------------------- policies

-- Published policies are immutable. Editing a live policy object is how a bad
-- AppLocker path reaches every machine with nothing to roll back to; here a
-- rollback is pointing policy_current at an older row.
--
-- content is the model.Policy JSON exactly as the agent will receive it.
-- Validation (ValidateAppLockerPath, interval clamping) stays in Go: the rules
-- are long, and duplicating them in SQL means two answers to one question.
create table policy_versions (
  version    bigserial primary key,
  content    jsonb not null,
  note       text not null default '',
  created_by text not null default '',
  created_at timestamptz not null default now()
);

-- Exactly one row, holding the version the fleet should be on. The singleton
-- check makes a second row impossible rather than merely unlikely.
create table policy_current (
  singleton  boolean primary key default true check (singleton),
  version    bigint not null references policy_versions(version),
  updated_at timestamptz not null default now(),
  updated_by text not null default ''
);

-- Global settings that are not policy: the quota a new account is pre-filled
-- with, alert channel references, feature switches. Key/value because they are
-- read one at a time by name and each has its own shape.
create table settings (
  key        text primary key check (key <> ''),
  value      jsonb not null,
  version    integer not null default 1 check (version > 0),
  updated_at timestamptz not null default now(),
  updated_by text not null default ''
);

-- ----------------------------------------------------------------- gateway

-- What the console wants to exist on the gateway, and what it last observed
-- there. Keeping desired and actual apart is the whole point: a provisioning
-- call that times out leaves us unsure, and a single "state" column would have
-- to lie in one direction or the other.
--
-- external_user is the gateway's user id (emp-<windows_user>). It is preserved
-- across a rename on purpose -- recreating the gateway user would orphan its
-- spend history.
create table gateway_grants (
  id            uuid primary key default gen_random_uuid(),
  employee_id   uuid not null references employees(id),
  epoch         integer not null check (epoch > 0),
  gateway       text not null default 'litellm' check (gateway <> ''),
  external_user text not null check (external_user <> ''),
  key_alias     text not null check (key_alias <> ''),
  models        text[] not null default '{}',
  desired       text not null check (desired in ('active', 'revoked')),
  -- 'unknown' until a call confirms it; 'missing' when the gateway says the
  -- key is not there, which is success for a revoke and a fault for an active
  -- grant.
  actual        text not null default 'unknown'
                check (actual in ('unknown', 'active', 'revoked', 'missing')),
  -- The issued token itself never lands here; it lives encrypted in
  -- credential_versions and this points at the row.
  credential_id uuid,
  reconciled_at timestamptz,
  last_error    text not null default '',
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  unique (gateway, key_alias)
);
create index gateway_grants_by_employee on gateway_grants (employee_id, epoch desc);
-- One live grant per employee per gateway. Two would mean two working tokens
-- for one person, and revoking "the" token would leave the other one alive.
create unique index gateway_grants_one_active
  on gateway_grants (gateway, employee_id) where desired = 'active';

-- Secrets we must keep: the gateway token delivered inside credentials.zip.
-- ciphertext is envelope-encrypted (internal/secrets); key_version records
-- which master key wrapped it so a rotation can still open old rows.
--
-- content_sha256 is the hash of the delivered bytes, not of the secret alone:
-- it answers "has the machine already got this?" without decrypting anything.
create table credential_versions (
  id             uuid primary key default gen_random_uuid(),
  employee_id    uuid not null references employees(id),
  epoch          integer not null check (epoch > 0),
  purpose        text not null check (purpose <> ''),   -- 'codex_gateway'
  ciphertext     bytea not null,
  key_version    text not null,
  content_sha256 bytea,
  created_at     timestamptz not null default now(),
  retired_at     timestamptz
);
create index credential_versions_by_employee
  on credential_versions (employee_id, epoch desc);
-- At most one live credential per employee and purpose. Re-issuing retires the
-- previous row in the same transaction.
create unique index credential_versions_one_live
  on credential_versions (employee_id, purpose) where retired_at is null;

alter table gateway_grants
  add constraint gateway_grants_credential_fk
  foreign key (credential_id) references credential_versions(id);

-- ------------------------------------------------------------------- tasks

-- Every side effect outside the database -- gateway calls, OSS exports, audit
-- shipping -- is a task committed in the same transaction as the business
-- change that asked for it. Nothing is attempted inline: a gateway call in the
-- request path either blocks the administrator or gets lost on a restart.
--
-- idempotency_key is what makes "at least once" safe. It is derived from the
-- change, not from the moment (employee id + epoch + kind), so a retry after
-- an ambiguous failure finds the existing row instead of provisioning twice.
--
-- payload holds references, never secrets: a credential_versions id, not a
-- token. Tasks are queried, dumped and pasted into tickets.
create table tasks (
  id              uuid primary key default gen_random_uuid(),
  kind            text not null check (kind <> ''),
  idempotency_key text not null unique,
  payload         jsonb not null default '{}'::jsonb,
  status          text not null default 'pending'
                  check (status in ('pending', 'running', 'retry_wait',
                                    'succeeded', 'failed', 'superseded')),
  attempts        integer not null default 0 check (attempts >= 0),
  max_attempts    integer not null default 10 check (max_attempts > 0),
  -- Lease, not lock: a Worker that dies holds nothing. Another Worker may take
  -- a task whose lease_until has passed, and the dead one's completion is
  -- rejected because it no longer owns the lease.
  lease_until     timestamptz,
  lease_owner     text,
  next_run_at     timestamptz not null default now(),
  last_error      text not null default '',
  -- The employee epoch this task was created for. If the employee has moved on
  -- (offboarded, re-issued) the task is superseded rather than applied.
  target_epoch    integer,
  employee_id     uuid references employees(id),
  device_id       uuid references devices(id),
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now(),
  finished_at     timestamptz
);
-- The claim query's index: runnable tasks in due order.
create index tasks_runnable on tasks (next_run_at)
  where status in ('pending', 'retry_wait');
create index tasks_open_by_employee on tasks (employee_id)
  where status in ('pending', 'running', 'retry_wait');

-- One row per execution. Kept after success as well as failure -- "it worked
-- on the fourth try, ninety minutes late" is the interesting case, and a table
-- that only holds failures cannot show it.
create table task_attempts (
  id           bigserial primary key,
  task_id      uuid not null references tasks(id) on delete cascade,
  attempt      integer not null check (attempt > 0),
  owner        text not null default '',
  started_at   timestamptz not null default now(),
  ended_at     timestamptz,
  outcome      text check (outcome in ('succeeded', 'failed', 'retry', 'superseded')),
  error_class  text not null default '',
  error_detail text not null default '',
  -- The upstream's own request id, when it gives one. It is what turns "the
  -- gateway rejected it" into something the gateway's operator can look up.
  external_ref text not null default ''
);
create index task_attempts_by_task on task_attempts (task_id, attempt);

-- ------------------------------------------------------------------- audit

-- Append-only: the application role is granted INSERT and SELECT and nothing
-- else (0002_roles.up.sql), so a bug cannot rewrite history and neither can a
-- stolen application credential.
--
-- event_id is generated here and reused as the id of the SLS audit copy, which
-- is what allows the two to be reconciled line by line (spec 9.4).
create table audit_events (
  event_id    uuid primary key default gen_random_uuid(),
  occurred_at timestamptz not null default now(),
  actor_type  text not null check (actor_type in ('admin', 'device', 'worker', 'system')),
  actor_id    text not null default '',
  action      text not null check (action <> ''),
  target_type text not null default '',
  target_id   text not null default '',
  -- Redacted values, as shown in the console. Secrets are replaced upstream,
  -- in eventlog.Redact, before they ever reach a column.
  before      jsonb,
  after       jsonb,
  result      text not null default 'ok',
  detail      jsonb,
  task_id     uuid,
  request_id  text not null default ''
);
create index audit_events_by_target on audit_events (target_type, target_id, occurred_at desc);
create index audit_events_recent on audit_events (occurred_at desc);

-- Where each event has been shipped. Delivery is at-least-once, so SLS may
-- hold duplicates; queries there deduplicate on event_id.
create table event_deliveries (
  event_id     uuid not null references audit_events(event_id) on delete cascade,
  target       text not null check (target <> ''),      -- 'sls_audit'
  written_at   timestamptz,
  confirmed_at timestamptz,
  attempts     integer not null default 0 check (attempts >= 0),
  last_error   text not null default '',
  primary key (event_id, target)
);
create index event_deliveries_pending on event_deliveries (target)
  where confirmed_at is null;

-- ------------------------------------------------------------ administrators

-- Console logins. Self-managed accounts replace the current "paste an OSS
-- AccessKey into the login form" flow: a session must not carry cloud
-- credentials, and revoking one person's access must not mean rotating a key
-- everybody shares.
create table admin_principals (
  id               uuid primary key default gen_random_uuid(),
  username         text not null unique check (username = lower(username) and username <> ''),
  email            text not null default '',
  -- Argon2id, encoded with its parameters so they can be raised later without
  -- invalidating existing hashes.
  password_hash    text not null,
  password_set_at  timestamptz not null default now(),
  -- TOTP seed and recovery codes are envelope-encrypted like any other stored
  -- secret. Null seed means "enrolment not finished"; such an account can log
  -- in only to complete it.
  totp_secret      bytea,
  totp_key_version text not null default '',
  totp_enrolled_at timestamptz,
  -- Hashes only. A recovery code that can be read out of the database is a
  -- second password sitting in plain sight.
  recovery_hashes  text[] not null default '{}',
  role             text not null check (role in ('viewer', 'operator', 'security', 'admin')),
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  last_login_at    timestamptz,
  disabled_at      timestamptz
);

-- Sessions are keyed by the SHA-256 of the cookie value, never by the value
-- itself: a database dump, a backup or a slow query log must not hand anybody
-- a working session.
create table admin_sessions (
  token_sha256 bytea primary key,
  principal_id uuid not null references admin_principals(id) on delete cascade,
  created_at   timestamptz not null default now(),
  -- Idle timeout and absolute lifetime are separate. Refreshing on activity
  -- alone would let one login last forever.
  expires_at   timestamptz not null,
  absolute_expires_at timestamptz not null,
  last_seen_at timestamptz not null default now(),
  created_ip   inet,
  user_agent   text not null default ''
);
create index admin_sessions_by_principal on admin_sessions (principal_id);
create index admin_sessions_expiry on admin_sessions (expires_at);

-- Failed attempts, for rate limiting and for noticing a password spray. Rows
-- are pruned by the Worker; nothing here is history worth keeping for a year.
create table admin_login_attempts (
  id         bigserial primary key,
  username   text not null default '',
  source_ip  inet,
  at         timestamptz not null default now(),
  outcome    text not null check (outcome in ('ok', 'bad_password', 'bad_totp',
                                              'disabled', 'unknown_user', 'locked'))
);
create index admin_login_attempts_recent on admin_login_attempts (username, at desc);
create index admin_login_attempts_by_ip on admin_login_attempts (source_ip, at desc);

-- ------------------------------------------------------------------ legacy

-- The bridge to the OSS world: which Windows user name and which host name a
-- given row used to be known by. The importer fills it, the compat exporter
-- reads it, and it stays after the cutover so an old audit line naming
-- "work1" can still be resolved.
create table legacy_id_map (
  kind       text not null check (kind in ('employee', 'device')),
  legacy_key text not null check (legacy_key <> ''),
  new_id     uuid not null,
  mapped_at  timestamptz not null default now(),
  primary key (kind, legacy_key)
);
