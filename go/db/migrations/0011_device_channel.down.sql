drop table credential_bundles;
drop table device_tokens;
alter table devices drop column log_tail_at;
alter table devices drop column log_tail;
alter table devices drop column reenrol_allowed_until;
alter table devices drop column enrolled_from;
alter table devices drop column enrolled_at;
alter table devices drop column channel;
