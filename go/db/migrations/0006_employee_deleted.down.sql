-- Fails if a deleted account's name has been reused; remove the deleted rows
-- first in that case.
drop index employees_external_id_live;
drop index employees_windows_user_live;
alter table employees add constraint employees_external_id_key unique (external_id);
alter table employees add constraint employees_windows_user_key unique (windows_user);
alter table employees drop constraint employees_deleted_only_when_offboarded;
alter table employees drop column deleted_at;
