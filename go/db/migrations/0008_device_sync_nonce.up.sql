-- "Sync now" from the console. The nonce goes into the machine's binding
-- object; a changed object is what a 1.2.16 agent's heartbeat notices, and
-- it runs a full sync at once instead of waiting out its interval.
alter table devices add column sync_nonce text not null default '';
