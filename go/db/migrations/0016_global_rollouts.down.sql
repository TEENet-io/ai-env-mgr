update release_rollouts set kind = 'release' where kind = 'global';
alter table release_rollouts drop constraint if exists release_rollouts_kind_check;
alter table release_rollouts add constraint release_rollouts_kind_check
  check (kind in ('release', 'rollback'));
