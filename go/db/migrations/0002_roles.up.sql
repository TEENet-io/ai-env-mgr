-- Three database accounts, so that the one the console runs as cannot do the
-- things the console never needs to do:
--
--   aienv_migrate  owns the schema and is the only account that runs DDL.
--                  Used by cmd/migrate, by hand, and never by the service.
--   aienv_app      the console and Worker. Reads and writes business tables;
--                  on audit_events and task_attempts it may only INSERT and
--                  SELECT, which is what "append-only" has to mean if it is to
--                  survive a bug or a stolen password.
--   aienv_ro       queries only. For investigation and for the reporting that
--                  should never be able to change what it is reporting on.
--
-- The roles are created without a password and without LOGIN here so this file
-- is safe to keep in git and safe to re-run. Passwords are set out of band,
-- once, on the instance:
--
--   alter role aienv_app  login password '<from the secret store>';
--   alter role aienv_ro   login password '<...>';
--   alter role aienv_migrate login password '<...>';
--
-- On Alibaba RDS the privileged account is not a true superuser; it can still
-- create roles and grant them, which is all this needs.

do $$
begin
  if not exists (select 1 from pg_roles where rolname = 'aienv_migrate') then
    create role aienv_migrate nologin;
  end if;
  if not exists (select 1 from pg_roles where rolname = 'aienv_app') then
    create role aienv_app nologin;
  end if;
  if not exists (select 1 from pg_roles where rolname = 'aienv_ro') then
    create role aienv_ro nologin;
  end if;
end
$$;

grant usage on schema public to aienv_migrate, aienv_app, aienv_ro;

-- Nobody creates tables outside a migration.
revoke create on schema public from public;
grant create on schema public to aienv_migrate;

-- Start from nothing, then hand out exactly what each account needs. Running
-- this file again after new tables appear re-applies the whole grid, which is
-- why the migration is written to be idempotent.
revoke all on all tables in schema public from aienv_app, aienv_ro;
revoke all on all sequences in schema public from aienv_app, aienv_ro;

grant select, insert, update, delete on all tables in schema public to aienv_app;
grant usage, select on all sequences in schema public to aienv_app;

-- Append-only tables: take back everything that could rewrite history.
revoke update, delete, truncate on audit_events, task_attempts from aienv_app;

grant select on all tables in schema public to aienv_ro;

-- Tables created by later migrations inherit the same grid, so a new table
-- does not silently arrive unreadable -- or, worse, writable by aienv_ro.
alter default privileges for role aienv_migrate in schema public
  grant select, insert, update, delete on tables to aienv_app;
alter default privileges for role aienv_migrate in schema public
  grant usage, select on sequences to aienv_app;
alter default privileges for role aienv_migrate in schema public
  grant select on tables to aienv_ro;
