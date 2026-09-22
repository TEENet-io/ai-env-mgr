-- The agent's own channel to the console. A machine that talks to the
-- console directly holds one device token (only its hash is here); the
-- ones still reading the bucket are channel 'oss'. The log tail moves in
-- from the bucket for the same reason the status did.
alter table devices add column channel text not null default 'oss' check (channel in ('oss', 'api'));
alter table devices add column enrolled_at timestamptz;
alter table devices add column enrolled_from text not null default '';
alter table devices add column reenrol_allowed_until timestamptz;
alter table devices add column log_tail text not null default '';
alter table devices add column log_tail_at timestamptz;

create table device_tokens (
  id           uuid primary key default gen_random_uuid(),
  device_id    uuid not null references devices (id),
  token_hash   bytea not null unique,
  created_at   timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at   timestamptz,
  -- After a rotation the old token keeps working until here, so an agent
  -- that lost the reply to its rotate request is not locked out.
  grace_until  timestamptz
);
create index device_tokens_by_device on device_tokens (device_id) where revoked_at is null;

-- The credentials bundle as delivered to a machine, per employee and epoch,
-- so the device API can hand it out without going through the bucket.
create table credential_bundles (
  employee_id uuid not null references employees (id),
  epoch       integer not null,
  zip         bytea not null,
  etag        text not null,
  created_at  timestamptz not null default now(),
  primary key (employee_id, epoch)
);
