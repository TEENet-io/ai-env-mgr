-- A condition somebody should hear about, with its lifetime: opened when a
-- rule first sees it, resolved when the rule no longer does (or by hand),
-- acknowledged by whoever is on it. One open row per fingerprint, so a rule
-- that fires every ten minutes does not open ten alerts for one silent
-- machine; notified_at is what keeps the one alert to one message.
create table alerts (
  id           uuid primary key default gen_random_uuid(),
  kind         text not null,
  fingerprint  text not null,
  severity     text not null check (severity in ('warn', 'crit')),
  subject_type text not null,
  subject_id   text not null,
  title        text not null,
  detail       text not null default '',
  opened_at    timestamptz not null default now(),
  resolved_at  timestamptz null,
  resolved_by  text not null default '',
  acked_at     timestamptz null,
  acked_by     text not null default '',
  notified_at  timestamptz null,
  notify_tries integer not null default 0,
  notify_error text not null default ''
);
create unique index alerts_one_open on alerts (fingerprint) where resolved_at is null;
create index alerts_open on alerts (opened_at desc) where resolved_at is null;
create index alerts_recent on alerts (opened_at desc);
