drop index if exists devices_hostname_lower;
alter table devices add constraint devices_hostname_key unique (hostname);
