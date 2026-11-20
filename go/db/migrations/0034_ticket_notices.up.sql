-- What a system-written event stands for ("plan_accepted", ...), so a
-- notice is chosen by what happened, not by words a customer could type.
-- Empty for ordinary comments and status changes.
alter table ticket_events add column action text not null default '';

-- Who has been told about a ticket event. ticket_events is append-only, so
-- delivery state lives beside it: one row per event and audience ("team" is
-- our chat group, "customer" the person who raised the ticket). An event
-- nobody needs to hear about still gets a row, marked skipped, so it is
-- decided once and not looked at again.
create table ticket_notices (
  event_id   bigint not null references ticket_events(id),
  audience   text not null check (audience in ('team', 'customer')),
  scenario   text not null default '',
  state      text not null default 'pending' check (state in ('pending', 'sent', 'skipped', 'failed')),
  tries      integer not null default 0,
  last_error text not null default '', -- for skipped: why nobody was told
  -- A run that takes a notice holds it until then; one that died mid-send
  -- leaves it to be taken again after the lease.
  lease_until timestamptz null,
  created_at timestamptz not null default now(),
  sent_at    timestamptz null,
  primary key (event_id, audience)
);
create index ticket_notices_pending on ticket_notices (audience, created_at) where state = 'pending';

-- What happened before this table existed is history, not news: a deploy
-- must not page the group about every ticket of the past day.
insert into ticket_notices (event_id, audience, state)
select e.id, a.audience, 'skipped'
  from ticket_events e cross join (values ('team'), ('customer')) as a(audience);
