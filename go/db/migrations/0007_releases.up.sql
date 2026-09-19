-- Release pipeline (spec 2026-09-18): what has been built, what has been
-- handed to which machine, and what came of it.

-- One row per package. (product, version) is unique and the sha256 is set
-- once: a version name refers to exactly one set of bytes, forever. A
-- rebuild with different content is a new version.
create table release_artifacts (
  id                uuid primary key default gen_random_uuid(),
  product           text not null check (product in ('agent', 'codex')),
  version           text not null check (version <> ''),
  sha256            text not null check (length(sha256) = 64),
  size_bytes        bigint not null check (size_bytes > 0),
  object_key        text not null check (object_key <> ''),
  status            text not null default 'candidate'
                    check (status in ('candidate', 'accepted', 'stable', 'retired')),
  notes             text not null default '',
  source            text not null default '',
  -- The oldest agent this package may be aimed at. For codex, the agent
  -- that installs it; for agent, the agent that replaces itself.
  min_agent_version text not null default '',
  acceptance_note   text not null default '',
  accepted_by       text not null default '',
  accepted_at       timestamptz,
  created_by        text not null default '',
  created_at        timestamptz not null default now(),
  updated_at        timestamptz not null default now(),
  unique (product, version)
);

-- One row per "hand this version to these machines" decision, including a
-- rollback, which is the same decision with an older version.
create table release_rollouts (
  id          uuid primary key default gen_random_uuid(),
  product     text not null check (product in ('agent', 'codex')),
  artifact_id uuid not null references release_artifacts(id),
  kind        text not null default 'release' check (kind in ('release', 'rollback')),
  rollback_of uuid references release_rollouts(id),
  note        text not null default '',
  created_by  text not null default '',
  created_at  timestamptz not null default now(),
  paused_at   timestamptz,
  paused_by   text not null default ''
);

-- Desired state, one row per device per product per decision. Not a task:
-- nothing on the server carries it out. The device pulls it and its report
-- settles it.
create table release_targets (
  id               uuid primary key default gen_random_uuid(),
  device_id        uuid not null references devices(id),
  product          text not null check (product in ('agent', 'codex')),
  artifact_id      uuid not null references release_artifacts(id),
  rollout_id       uuid not null references release_rollouts(id),
  -- Counts targets per (device, product). The agent echoes it back, so a
  -- receipt for an earlier generation cannot settle a later one.
  generation       integer not null check (generation > 0),
  status           text not null default 'pending'
                   check (status in ('pending', 'succeeded', 'failed', 'cancelled', 'superseded', 'excluded')),
  result_note      text not null default '',
  reported_version text not null default '',
  exclude_reason   text not null default '',
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  finished_at      timestamptz,
  unique (device_id, product, generation),
  constraint release_targets_finished_matches_status
    check ((status = 'pending') = (finished_at is null))
);
-- The rule itself: one open target per device and product.
create unique index release_targets_one_open
  on release_targets (device_id, product) where status = 'pending';
create index release_targets_by_rollout on release_targets (rollout_id);
