-- One row per day, token alias and model: what the gateway logs said an
-- employee spent. The log service keeps events for a limited time; this
-- table is what makes last quarter's bill answerable. employee_id is null
-- when the alias in the log matched no grant and no account -- the spend
-- still counts, it just has nobody to hang on.
create table usage_daily (
  day               date          not null,
  employee_id       uuid          null references employees (id),
  key_alias         text          not null,
  model_group       text          not null default '',
  calls             integer       not null default 0,
  failures          integer       not null default 0,
  cost_usd          numeric(12,6) not null default 0,
  unpriced          integer       not null default 0,
  prompt_tokens     bigint        not null default 0,
  completion_tokens bigint        not null default 0,
  updated_at        timestamptz   not null default now(),
  primary key (day, key_alias, model_group)
);
create index usage_daily_by_employee on usage_daily (employee_id, day desc);
