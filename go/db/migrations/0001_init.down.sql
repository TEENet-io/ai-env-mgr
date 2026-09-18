-- Reverse of 0001_init.up.sql. Dropping in dependency order rather than with
-- CASCADE: if something outside this migration has come to depend on a table,
-- the drop should fail loudly instead of taking that something with it.

drop table if exists legacy_id_map;
drop table if exists admin_login_attempts;
drop table if exists admin_sessions;
drop table if exists admin_principals;
drop table if exists event_deliveries;
drop table if exists audit_events;
drop table if exists task_attempts;
drop table if exists tasks;

alter table if exists gateway_grants drop constraint if exists gateway_grants_credential_fk;
drop table if exists credential_versions;
drop table if exists gateway_grants;

drop table if exists settings;
drop table if exists policy_current;
drop table if exists policy_versions;

drop table if exists device_reported_state;
drop table if exists device_bindings;
drop table if exists devices;

drop table if exists employee_quotas;
drop table if exists employee_models;
drop table if exists employees;
