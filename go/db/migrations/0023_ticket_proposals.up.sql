-- Proposed deletion scope or quota grant is separate from the request and
-- from the execution result. A decision is append-only and applies to one
-- exact proposal, never to an unspecified later amendment.
create table ticket_proposals (
  id bigint generated always as identity primary key,
  ticket_id uuid not null references tickets(id),
  kind text not null check (kind in ('account-delete','quota')),
  payload jsonb not null,
  requires_manager boolean not null,
  admin_id uuid not null references admin_principals(id),
  created_at timestamptz not null default now()
);
create index ticket_proposals_latest on ticket_proposals(ticket_id,id desc);

create table ticket_proposal_decisions (
  id bigint generated always as identity primary key,
  proposal_id bigint not null unique references ticket_proposals(id),
  contact_id uuid not null references customer_contacts(id),
  decision text not null check (decision in ('approved','rejected')),
  created_at timestamptz not null default now()
);
