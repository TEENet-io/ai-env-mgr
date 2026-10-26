-- Admin-owned jobs are the long operations that are started by an HTTP
-- request but are not executed by the device Worker: package hashing,
-- publishing and application catalog uploads. Keeping them in PostgreSQL
-- means a console restart no longer erases the only progress record.
create table admin_jobs (
  id          text primary key,
  kind        text not null check (kind <> ''),
  version     text not null default '',
  state       text not null check (state in ('running', 'done', 'failed')),
  step        text not null default '',
  done_bytes  bigint not null default 0 check (done_bytes >= 0),
  total_bytes bigint not null default 0 check (total_bytes >= 0),
  last_error  text not null default '',
  started_at  timestamptz not null default now(),
  ended_at    timestamptz,
  updated_at  timestamptz not null default now()
);

create index admin_jobs_recent on admin_jobs (updated_at desc);

