-- Keep each customer-submitted version of a daily service request. Assignment
-- and ordinary comments advance tickets.version, but not request_version.
alter table tickets add column request_version integer not null default 1 check (request_version > 0);

create table ticket_request_revisions (
  id bigint generated always as identity primary key,
  ticket_id uuid not null references tickets(id),
  request_version integer not null check (request_version > 0),
  title text not null,
  description text not null,
  details jsonb not null,
  actor_id uuid not null references customer_contacts(id),
  created_at timestamptz not null default now(),
  unique (ticket_id, request_version)
);
create index ticket_request_revisions_ticket on ticket_request_revisions(ticket_id, request_version desc);

insert into ticket_request_revisions(ticket_id,request_version,title,description,details,actor_id,created_at)
select id,1,title,description,details,requester_id,created_at
from tickets where requester_id is not null and details ? 'request_kind';
