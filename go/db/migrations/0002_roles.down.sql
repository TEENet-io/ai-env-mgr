-- Hands back the privileges 0002 granted. The roles themselves are left in
-- place: they may own objects, hold grants in other databases, or be in use by
-- a session right now, and a migration that can drop the account the service
-- logs in with is a migration that can take the console down.
--
-- Drop them by hand, after checking, if they are really unwanted:
--   drop owned by aienv_app; drop role aienv_app;

alter default privileges for role aienv_migrate in schema public
  revoke select, insert, update, delete on tables from aienv_app;
alter default privileges for role aienv_migrate in schema public
  revoke usage, select on sequences from aienv_app;
alter default privileges for role aienv_migrate in schema public
  revoke select on tables from aienv_ro;

revoke all on all tables in schema public from aienv_app, aienv_ro;
revoke all on all sequences in schema public from aienv_app, aienv_ro;
revoke usage on schema public from aienv_migrate, aienv_app, aienv_ro;
revoke create on schema public from aienv_migrate;

grant create on schema public to public;
