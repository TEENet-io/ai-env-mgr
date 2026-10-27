do $$ begin
  if exists (select 1 from tickets where requester_id is null) then
    raise exception 'cannot roll back internal tickets while they exist';
  end if;
end $$;
drop index tickets_internal_request_key;
alter table tickets alter column requester_id set not null;
