-- The customer's custom tool becomes the one list of people (AIC-206). The
-- console keeps a copy of that list, refreshed by the Worker, and links its
-- own accounts to it by the tool's account id, which never changes and is
-- never reused.
--
-- status is what the tool last said. Only an explicit 'disabled' or
-- 'deleted' takes anything away; an account missing from a reply is left
-- as it was (missing_since records it), because a truncated or broken reply
-- must not look like everybody leaving.
create table directory_accounts (
  id            uuid primary key,
  username      text not null check (username <> ''),
  name          text not null default '',
  email         text not null default '',
  role          text not null check (role in ('ops', 'admin', 'employee')),
  status        text not null check (status in ('active', 'disabled', 'deleted')),
  first_seen_at timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  missing_since timestamptz
);

-- What the console's own records belong to. Unique while set: one person,
-- one contact, one console account, one AI workspace account.
alter table customer_contacts add column directory_id uuid references directory_accounts(id);
create unique index customer_contacts_directory_id on customer_contacts(directory_id) where directory_id is not null;
alter table admin_principals add column directory_id uuid references directory_accounts(id);
create unique index admin_principals_directory_id on admin_principals(directory_id) where directory_id is not null;
alter table employees add column directory_id uuid references directory_accounts(id);
create unique index employees_directory_id_live on employees(directory_id) where directory_id is not null and deleted_at is null;

-- A person who signs in through the custom tool has no password here.
alter table customer_contacts alter column password_hash drop not null;
alter table admin_principals alter column password_hash drop not null;

-- One row: when the list was last read, and what went wrong if it was not.
-- The alerts read it; the sync is the only writer.
create table directory_sync_state (
  id                 boolean primary key default true check (id),
  last_success_at    timestamptz,
  last_attempt_at    timestamptz,
  last_error         text not null default '',
  failing_since      timestamptz,
  held_disables      jsonb
);
insert into directory_sync_state default values;
