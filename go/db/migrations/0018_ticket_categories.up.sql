-- Keep legacy ticket types readable; new requests use general or bug.
alter table tickets drop constraint tickets_type_check;
alter table tickets add constraint tickets_type_check
  check (type in ('onboarding', 'offboarding', 'access', 'other', 'general', 'bug'));
