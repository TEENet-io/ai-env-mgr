-- Refuses rollback if customer audit events exist: history must not be rewritten.
alter table audit_events drop constraint audit_events_actor_type_check;
alter table audit_events add constraint audit_events_actor_type_check
  check (actor_type in ('admin', 'device', 'worker', 'system'));
