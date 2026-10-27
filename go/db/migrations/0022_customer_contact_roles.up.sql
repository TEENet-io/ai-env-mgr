alter table customer_contacts add column role text not null default 'manager'
  check (role in ('manager','employee'));
