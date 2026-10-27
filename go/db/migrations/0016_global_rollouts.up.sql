-- A global target is still recorded as a normal per-machine target for
-- modern agents, but it must not pin the machine after it succeeds. The
-- fleet policy remains the source of truth for machines that follow global.
alter table release_rollouts drop constraint if exists release_rollouts_kind_check;
alter table release_rollouts add constraint release_rollouts_kind_check
  check (kind in ('release', 'rollback', 'global'));
