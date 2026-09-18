-- The first read of the live gateway found rate limits of 99999999999999,
-- set by hand to mean "effectively unlimited". They are legitimate values on
-- the gateway, and a column that cannot hold what the gateway holds would
-- make the import refuse a whole account over it.

alter table employee_quotas
  alter column rpm type bigint,
  alter column tpm type bigint,
  alter column parallel type bigint;
