-- A viewer goes back to the narrower of the two old roles.
update customer_contacts set role = 'employee' where role = 'viewer';
alter table customer_contacts drop constraint customer_contacts_role_check;
alter table customer_contacts add constraint customer_contacts_role_check
  check (role in ('manager', 'employee'));
alter table directory_accounts drop column ticket_role;
