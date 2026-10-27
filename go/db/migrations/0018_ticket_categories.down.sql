-- PostgreSQL cannot drop the new values while tickets still reference them.
-- Preserve customer history rather than silently deleting or rewriting it.
do $$ begin
  if exists (select 1 from tickets where type in ('general', 'bug')) then
    raise exception 'cannot roll back ticket categories while new tickets exist';
  end if;
end $$;
alter table tickets drop constraint tickets_type_check;
alter table tickets add constraint tickets_type_check
  check (type in ('onboarding', 'offboarding', 'access', 'other'));
