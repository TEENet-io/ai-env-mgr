-- Every employee in the custom tool is a viewer in the ticket system: it
-- reads the company's tickets, usage and pending proposals, and raises what
-- an employee may. Contacts the directory made before this were employees;
-- move them over. Contacts made by hand keep their role.
update customer_contacts set role = 'viewer'
  where role = 'employee'
    and directory_id in (select id from directory_accounts where role = 'employee');

-- An account the tool gave no email got no contact, and could not open the
-- ticket system at all. Contacts are now filed under the username for those
-- (dbstore.contactKey); give every live customer account that has none one.
-- A key someone else already holds is skipped, as a reading would.
insert into customer_contacts (email, name, password_hash, role, directory_id)
select coalesce(nullif(d.email, ''), lower(d.username)),
       coalesce(nullif(d.name, ''), d.username),
       null,
       case when d.role = 'admin' and d.ticket_role <> 'view' then 'manager' else 'viewer' end,
       d.id
  from directory_accounts d
 where d.status = 'active' and d.role in ('admin', 'employee') and d.username <> ''
   and not exists (select 1 from customer_contacts c where c.directory_id = d.id)
on conflict do nothing;
