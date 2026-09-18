-- The privilege grid, re-applied by the migration runner after every batch of
-- migrations. It is not a migration itself: a migration runs once, and a
-- grant made once does not cover a table created later. The account that
-- runs migrations creates the tables, so "default privileges" are set for
-- that account -- whichever it is -- rather than for a role that never
-- creates anything.
--
--   aienv_app   the console and Worker: reads and writes business tables;
--               append-only on audit_events and task_attempts; read-only on
--               schema_migrations, because the account that runs the code
--               must not be able to rewrite the record of which schema it
--               is running on.
--   aienv_ro    queries only.
--
-- Both are created NOLOGIN and without a password so this file is safe to
-- keep in git and to run again; passwords are set out of band, once:
--   alter role aienv_app login password '...';

do $$
begin
  if not exists (select 1 from pg_roles where rolname = 'aienv_app') then
    create role aienv_app nologin;
  end if;
  if not exists (select 1 from pg_roles where rolname = 'aienv_ro') then
    create role aienv_ro nologin;
  end if;
end
$$;

grant usage on schema public to aienv_app, aienv_ro;
revoke create on schema public from public;

revoke all on all tables in schema public from aienv_app, aienv_ro;
revoke all on all sequences in schema public from aienv_app, aienv_ro;

grant select, insert, update, delete on all tables in schema public to aienv_app;
grant usage, select on all sequences in schema public to aienv_app;
revoke update, delete, truncate on audit_events, task_attempts from aienv_app;
revoke insert, update, delete, truncate on schema_migrations from aienv_app;

grant select on all tables in schema public to aienv_ro;

-- Whatever the migrating account creates from now on inherits the grid.
alter default privileges in schema public grant select, insert, update, delete on tables to aienv_app;
alter default privileges in schema public grant usage, select on sequences to aienv_app;
alter default privileges in schema public grant select on tables to aienv_ro;
