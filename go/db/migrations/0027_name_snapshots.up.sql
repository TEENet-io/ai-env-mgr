-- The name a person had when a machine was bound to them or a day's usage
-- was counted (AIC-209 #8). Names now follow the custom tool, so a rename
-- there must not rewrite what was recorded under the old one. Existing rows
-- take the name they have today: the best there is.
alter table device_bindings add column employee_name text not null default '';
update device_bindings b set employee_name = e.name from employees e where e.id = b.employee_id;

alter table usage_daily add column employee_name text not null default '';
update usage_daily u set employee_name = e.name from employees e where e.id = u.employee_id;
