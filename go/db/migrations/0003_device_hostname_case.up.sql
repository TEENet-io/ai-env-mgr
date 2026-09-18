-- Windows host names are case-insensitive: DESKTOP-01 and desktop-01 are one
-- machine. A plain unique constraint let both be registered, which would mean
-- two device rows, two bindings and two sets of credentials aimed at one desk.
--
-- The stored spelling is left alone on purpose. The agent's OSS keys --
-- _bindings/<machine>, _status/<machine> -- use the name as the agent reports
-- it, so the console has to be able to reproduce it exactly.

alter table devices drop constraint devices_hostname_key;
create unique index devices_hostname_lower on devices (lower(hostname));
