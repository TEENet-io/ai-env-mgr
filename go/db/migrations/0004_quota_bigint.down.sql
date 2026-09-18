-- Narrowing fails if any row holds a value past int4, which is the point:
-- the down must not silently truncate a limit.
alter table employee_quotas
  alter column rpm type integer,
  alter column tpm type integer,
  alter column parallel type integer;
