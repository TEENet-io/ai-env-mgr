create table application_tasks (
  id text primary key,
  device_id uuid not null references devices(id),
  app_id text not null,
  version text not null,
  allow_downgrade boolean not null default false,
  state text not null check (state in ('pending','running','succeeded','failed','cancelled')),
  attempts integer not null default 0,
  lease_token text not null default '',
  lease_until timestamptz,
  progress text not null default '',
  last_error text not null default '',
  created_by text not null default '',
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  finished_at timestamptz
);

create unique index application_tasks_one_open_per_app
  on application_tasks (device_id, app_id) where state in ('pending','running');
create index application_tasks_device_queue
  on application_tasks (device_id, created_at) where state in ('pending','running');
