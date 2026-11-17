-- An ops account of the custom tool that tried the operators' entry before
-- anybody here opened it (AIC-206). The console lets in only the ops
-- accounts an administrator has bound to a console account; this is the
-- list of who is waiting for that, so binding them is one click on /admins.
--
-- Not a foreign key to directory_accounts: somebody created in the tool a
-- minute ago is not in the copy of the list until the next reading. Nothing
-- here grants anything; a row only puts a name on the page.
create table ops_signin_requests (
  directory_id  uuid primary key,
  username      text not null,
  name          text not null default '',
  email         text not null default '',
  first_at      timestamptz not null default now(),
  last_at       timestamptz not null default now(),
  attempts      integer not null default 1
);
