update customer_contacts c set role = case when d.ticket_role = 'view' then 'viewer' else 'manager' end
  from directory_accounts d
 where c.directory_id = d.id and d.role = 'admin';
update customer_contacts c set role = 'viewer'
  from directory_accounts d
 where c.directory_id = d.id and d.role = 'employee';
