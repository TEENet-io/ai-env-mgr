-- A machine that took a version through a rollout is kept on it afterwards
-- (the newest succeeded target). "跟随全局" on the machine page releases
-- that hold: released targets are history and no longer pin the machine.
alter table release_targets add column released_at timestamptz;
