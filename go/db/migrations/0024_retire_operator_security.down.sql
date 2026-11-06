-- Allows the old roles again. Accounts moved by the up migration stay viewers
-- and stay disabled; the audit trail records what each one was.
alter table admin_principals drop constraint admin_principals_role_check;
alter table admin_principals add constraint admin_principals_role_check check (role in ('viewer', 'operator', 'security', 'admin'));
