-- Admins may create an internal ticket without inventing a customer identity.
-- Such tickets remain visible only through the administrator console.
alter table tickets alter column requester_id drop not null;
create unique index tickets_internal_request_key on tickets(request_key) where requester_id is null;
