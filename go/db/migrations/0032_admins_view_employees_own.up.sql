-- The custom tool no longer lets a customer admin approve: every admin is a
-- viewer, and employees see only their own again (0031 had made them
-- viewers). Contacts made by hand keep their role.
update customer_contacts c set role = case d.role when 'admin' then 'viewer' else 'employee' end
  from directory_accounts d
 where c.directory_id = d.id and d.role in ('admin', 'employee')
   and c.role is distinct from case d.role when 'admin' then 'viewer' else 'employee' end;
