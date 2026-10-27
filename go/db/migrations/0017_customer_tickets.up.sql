-- One deployment serves one customer company. Customer identities never share
-- administrator sessions or privileges.
create table customer_contacts (
  id uuid primary key default gen_random_uuid(),
  email text not null unique check (email = lower(email) and email <> ''),
  name text not null check (name <> ''),
  password_hash text not null,
  created_at timestamptz not null default now(),
  last_login_at timestamptz,
  disabled_at timestamptz
);

create table customer_sessions (
  token_sha256 bytea primary key,
  contact_id uuid not null references customer_contacts(id) on delete cascade,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  absolute_expires_at timestamptz not null,
  last_seen_at timestamptz not null default now()
);
create index customer_sessions_contact on customer_sessions (contact_id);
create index customer_sessions_expiry on customer_sessions (expires_at);

create table customer_login_attempts (
  id bigserial primary key,
  email text not null,
  source_key text not null,
  at timestamptz not null default now(),
  succeeded boolean not null
);
create index customer_login_attempts_recent on customer_login_attempts (email, source_key, at desc);
create index customer_login_attempts_source on customer_login_attempts (source_key, at desc);

create table tickets (
  id uuid primary key default gen_random_uuid(),
  number bigint generated always as identity unique,
  requester_id uuid not null references customer_contacts(id),
  request_key text not null,
  type text not null check (type in ('onboarding', 'offboarding', 'access', 'other')),
  title text not null check (length(title) between 1 and 160),
  description text not null check (length(description) between 1 and 10000),
  details jsonb not null default '{}'::jsonb,
  status text not null default 'new' check (status in ('new', 'working', 'waiting_customer', 'done', 'closed')),
  priority text not null default 'normal' check (priority in ('normal', 'urgent')),
  assignee_id uuid references admin_principals(id),
  employee_id uuid references employees(id),
  device_id uuid references devices(id),
  version integer not null default 1 check (version > 0),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  closed_at timestamptz
);
create index tickets_queue on tickets (status, updated_at desc);
create unique index tickets_request_key on tickets (requester_id, request_key);
create index tickets_requester on tickets (requester_id, updated_at desc);

-- Events, including comments, are append-only. A public event is visible to
-- the requester; an internal event is not. No sensitive task payloads here.
create table ticket_events (
  id bigserial primary key,
  ticket_id uuid not null references tickets(id),
  actor_type text not null check (actor_type in ('customer', 'admin', 'system')),
  actor_id uuid,
  kind text not null check (kind in ('created', 'comment', 'status', 'assignment', 'link')),
  visibility text not null check (visibility in ('public', 'internal')),
  body text not null default '',
  from_status text,
  to_status text,
  created_at timestamptz not null default now()
);
create index ticket_events_timeline on ticket_events (ticket_id, id);

create table ticket_task_links (
  ticket_id uuid not null references tickets(id),
  task_kind text not null check (task_kind in ('task', 'application', 'admin')),
  task_id text not null check (length(task_id) between 1 and 200),
  linked_by uuid not null references admin_principals(id),
  created_at timestamptz not null default now(),
  primary key (ticket_id, task_kind, task_id)
);

-- Migrate() reapplies the role grid after all migrations; this revocation is
-- repeated there so a later migration cannot accidentally make history mutable.
