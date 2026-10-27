-- Preserve every implementation update and customer confirmation as an
-- append-only record; the original customer application remains immutable.
create table ticket_change_entries (
  id bigint generated always as identity primary key,
  ticket_id uuid not null references tickets(id),
  kind text not null check (kind in ('implementation', 'confirmation')),
  actor_type text not null check (actor_type in ('admin', 'customer')),
  actor_id uuid not null,
  payload jsonb not null,
  created_at timestamptz not null default now(),
  check ((kind='implementation' and actor_type='admin') or
         (kind='confirmation' and actor_type='customer'))
);
create index ticket_change_entries_latest on ticket_change_entries(ticket_id, id desc);
