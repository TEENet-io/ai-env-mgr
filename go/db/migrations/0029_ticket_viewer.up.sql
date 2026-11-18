-- A customer admin who may look but not approve (AIC-207). The custom tool
-- sends ticket_role with each admin: 'approve' may raise account and quota
-- requests and confirm proposals, which cost money; 'view' sees the
-- company's usage, employees and pending proposals, and otherwise has what
-- an employee has. Other roles carry ''.
alter table directory_accounts add column ticket_role text not null default ''
  check (ticket_role in ('', 'approve', 'view'));
-- Admins already on the copy were approving; say so, or the first reading
-- after this would report every one of them as a role change.
update directory_accounts set ticket_role = 'approve' where role = 'admin';

alter table customer_contacts drop constraint customer_contacts_role_check;
alter table customer_contacts add constraint customer_contacts_role_check
  check (role in ('manager', 'viewer', 'employee'));
