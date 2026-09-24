-- The operations process (ai工作间流程 §7) opens an account with a user
-- name and an email: the address the cloud desktop's verification codes go
-- to. Kept as a note next to the name; nothing here sends to it.
alter table employees add column email text not null default '';
