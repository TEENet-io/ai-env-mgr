-- Only admin and viewer remain. An account left with a retired role is
-- disabled rather than deleted, because tickets and the audit trail refer to
-- it. It becomes a viewer so that re-enabling it can never grant more than
-- reading. Its username and the role it had stay in the audit trail.
with recorded as (
  insert into audit_events (actor_type, actor_id, action, target_type, target_id, result, before, after)
  select 'system', 'migration-0024', 'admin_role_retired', 'admin', username, 'ok',
         jsonb_build_object('role', role, 'disabled', disabled_at is not null),
         jsonb_build_object('role', 'viewer', 'disabled', true)
    from admin_principals
   where role in ('operator', 'security')
  returning event_id
)
insert into event_deliveries (event_id, target)
select event_id, 'sls_audit' from recorded;

delete from admin_sessions
 where principal_id in (select id from admin_principals where role in ('operator', 'security'));

update admin_principals
   set role = 'viewer', disabled_at = coalesce(disabled_at, now()), updated_at = now()
 where role in ('operator', 'security');

alter table admin_principals drop constraint admin_principals_role_check;
alter table admin_principals add constraint admin_principals_role_check check (role in ('viewer', 'admin'));
