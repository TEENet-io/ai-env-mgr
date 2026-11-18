-- The contacts filed under a username stay: tickets may point at them.
update customer_contacts set role = 'employee'
  where role = 'viewer'
    and directory_id in (select id from directory_accounts where role = 'employee');
