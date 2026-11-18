-- A short name people use for a machine (BPO1, BPO2): the hostname is the
-- one the cloud gave it and cannot change, and a row of DESKTOP-7F3K... is
-- hard to tell apart. Unique ignoring case while set, so two machines are
-- never both BPO1. The free-form remark goes in the existing note column.
alter table devices add column label text not null default ''
  check (length(label) <= 32);
create unique index devices_label_lower on devices (lower(label)) where label <> '';
