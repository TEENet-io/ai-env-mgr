-- Accounts with no password cannot be kept under the old constraint; they
-- are removed with the links that made them. Tickets keep their requester
-- through the contact row, so contacts made by the sync are disabled and
-- given an unusable hash rather than deleted.
update customer_contacts set password_hash = '!', disabled_at = coalesce(disabled_at, now()) where password_hash is null;
update admin_principals set password_hash = '!', disabled_at = coalesce(disabled_at, now()) where password_hash is null;
alter table customer_contacts alter column password_hash set not null;
alter table admin_principals alter column password_hash set not null;

drop table directory_sync_state;
drop index employees_directory_id_live;
alter table employees drop column directory_id;
drop index admin_principals_directory_id;
alter table admin_principals drop column directory_id;
drop index customer_contacts_directory_id;
alter table customer_contacts drop column directory_id;
drop table directory_accounts;
