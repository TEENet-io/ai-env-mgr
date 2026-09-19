-- An account can be deleted once it is offboarded (decided 2026-09-19). It
-- leaves every list and its Windows user name becomes free for a new person.
-- The row stays: binding history and the audit trail still resolve to it.
alter table employees add column deleted_at timestamptz;
alter table employees add constraint employees_deleted_only_when_offboarded
  check (deleted_at is null or status = 'offboarded');

-- Uniqueness now applies among the living only.
alter table employees drop constraint employees_windows_user_key;
alter table employees drop constraint employees_external_id_key;
create unique index employees_windows_user_live
  on employees (windows_user) where deleted_at is null;
create unique index employees_external_id_live
  on employees (external_id) where deleted_at is null and external_id is not null;
