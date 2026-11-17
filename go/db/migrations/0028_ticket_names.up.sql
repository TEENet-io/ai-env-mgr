-- Who raised a ticket, and who wrote each customer entry, as they were
-- called at the time (AIC-204 #6). Contacts now take their names from the
-- custom tool, so a rename there must not rewrite tickets already raised.
-- requester_account is the tool's user name, or the email of a contact the
-- tool does not know. Existing rows take today's values: the best there is.
alter table tickets add column requester_name text not null default '';
alter table tickets add column requester_account text not null default '';
update tickets t set requester_name = c.name, requester_account = coalesce(d.username, c.email)
  from customer_contacts c left join directory_accounts d on d.id = c.directory_id
  where c.id = t.requester_id;

alter table ticket_events add column actor_name text not null default '';
update ticket_events e set actor_name = c.name from customer_contacts c
  where e.actor_type = 'customer' and c.id = e.actor_id;
