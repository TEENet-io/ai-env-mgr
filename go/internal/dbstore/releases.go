package dbstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type releaseRepo struct{ q querier }

const artifactColumns = `id, product, version, sha256, size_bytes, object_key, status, notes, source,
	min_agent_version, acceptance_note, accepted_by, accepted_at, created_by, created_at, updated_at`

func scanArtifact(row scanner) (repo.Artifact, error) {
	var a repo.Artifact
	err := row.Scan(&a.ID, &a.Product, &a.Version, &a.SHA256, &a.SizeBytes, &a.ObjectKey, &a.Status,
		&a.Notes, &a.Source, &a.MinAgentVersion, &a.AcceptanceNote, &a.AcceptedBy, &a.AcceptedAt,
		&a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func (r releaseRepo) CreateArtifact(ctx context.Context, n repo.NewArtifact) (repo.Artifact, error) {
	a, err := scanArtifact(r.q.QueryRow(ctx,
		`insert into release_artifacts
		   (product, version, sha256, size_bytes, object_key, notes, source, min_agent_version, created_by)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 returning `+artifactColumns,
		n.Product, n.Version, n.SHA256, n.SizeBytes, n.ObjectKey, n.Notes, n.Source, n.MinAgentVersion, n.CreatedBy))
	if err != nil {
		return repo.Artifact{}, mapError(err, "register artifact")
	}
	return a, nil
}

func (r releaseRepo) ArtifactByID(ctx context.Context, id string) (repo.Artifact, error) {
	a, err := scanArtifact(r.q.QueryRow(ctx, `select `+artifactColumns+` from release_artifacts where id = $1`, id))
	if err != nil {
		return repo.Artifact{}, mapError(err, "read artifact")
	}
	return a, nil
}

func (r releaseRepo) ArtifactByVersion(ctx context.Context, product, version string) (repo.Artifact, error) {
	a, err := scanArtifact(r.q.QueryRow(ctx,
		`select `+artifactColumns+` from release_artifacts where product = $1 and version = $2`, product, version))
	if err != nil {
		return repo.Artifact{}, mapError(err, "read artifact")
	}
	return a, nil
}

func (r releaseRepo) ListArtifacts(ctx context.Context, product string) ([]repo.Artifact, error) {
	rows, err := r.q.Query(ctx,
		`select `+artifactColumns+` from release_artifacts
		  where $1 = '' or product = $1
		  order by created_at desc`, product)
	if err != nil {
		return nil, mapError(err, "list artifacts")
	}
	defer rows.Close()
	out := []repo.Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, mapError(err, "list artifacts")
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r releaseRepo) SetArtifactStatus(ctx context.Context, id string, status repo.ArtifactStatus, note, by string) (repo.Artifact, error) {
	a, err := scanArtifact(r.q.QueryRow(ctx,
		`update release_artifacts set
		   status = $2,
		   acceptance_note = case when $2 in ('accepted', 'stable') then $3 else acceptance_note end,
		   accepted_by     = case when $2 in ('accepted', 'stable') and accepted_at is null then $4 else accepted_by end,
		   accepted_at     = case when $2 in ('accepted', 'stable') and accepted_at is null then now() else accepted_at end,
		   updated_at = now()
		 where id = $1
		 returning `+artifactColumns, id, string(status), note, by))
	if err != nil {
		return repo.Artifact{}, mapError(err, "set artifact status")
	}
	return a, nil
}

const rolloutColumns = `id, product, artifact_id, kind, coalesce(rollback_of::text, ''), note, created_by, created_at, paused_at, paused_by`

func scanRollout(row scanner) (repo.Rollout, error) {
	var ro repo.Rollout
	err := row.Scan(&ro.ID, &ro.Product, &ro.ArtifactID, &ro.Kind, &ro.RollbackOf, &ro.Note, &ro.CreatedBy,
		&ro.CreatedAt, &ro.PausedAt, &ro.PausedBy)
	return ro, err
}

func (r releaseRepo) CreateRollout(ctx context.Context, n repo.NewRollout) (repo.Rollout, error) {
	var rollbackOf *string
	if n.RollbackOf != "" {
		rollbackOf = &n.RollbackOf
	}
	ro, err := scanRollout(r.q.QueryRow(ctx,
		`insert into release_rollouts (product, artifact_id, kind, rollback_of, note, created_by)
		 values ($1, $2, $3, $4, $5, $6) returning `+rolloutColumns,
		n.Product, n.ArtifactID, string(n.Kind), rollbackOf, n.Note, n.CreatedBy))
	if err != nil {
		return repo.Rollout{}, mapError(err, "create rollout")
	}
	return ro, nil
}

func (r releaseRepo) RolloutByID(ctx context.Context, id string) (repo.Rollout, error) {
	ro, err := scanRollout(r.q.QueryRow(ctx, `select `+rolloutColumns+` from release_rollouts where id = $1`, id))
	if err != nil {
		return repo.Rollout{}, mapError(err, "read rollout")
	}
	return ro, nil
}

func (r releaseRepo) ListRollouts(ctx context.Context, limit int) ([]repo.Rollout, error) {
	rows, err := r.q.Query(ctx, `select `+rolloutColumns+` from release_rollouts order by created_at desc limit $1`, limit)
	if err != nil {
		return nil, mapError(err, "list rollouts")
	}
	defer rows.Close()
	out := []repo.Rollout{}
	for rows.Next() {
		ro, err := scanRollout(rows)
		if err != nil {
			return nil, mapError(err, "list rollouts")
		}
		out = append(out, ro)
	}
	return out, rows.Err()
}

func (r releaseRepo) SetRolloutPaused(ctx context.Context, id string, paused bool, by string) (repo.Rollout, error) {
	ro, err := scanRollout(r.q.QueryRow(ctx,
		`update release_rollouts set
		   paused_at = case when $2 then now() else null end,
		   paused_by = case when $2 then $3 else '' end
		 where id = $1 returning `+rolloutColumns, id, paused, by))
	if err != nil {
		return repo.Rollout{}, mapError(err, "pause rollout")
	}
	return ro, nil
}

const targetColumns = `id, device_id, product, artifact_id, rollout_id, generation, status, result_note,
	reported_version, exclude_reason, created_at, updated_at, finished_at`

func scanTarget(row scanner) (repo.Target, error) {
	var t repo.Target
	err := row.Scan(&t.ID, &t.DeviceID, &t.Product, &t.ArtifactID, &t.RolloutID, &t.Generation, &t.Status,
		&t.ResultNote, &t.ReportedVersion, &t.ExcludeReason, &t.CreatedAt, &t.UpdatedAt, &t.FinishedAt)
	return t, err
}

// CreateTarget supersedes the open target first and inserts second. Two
// statements rather than one: inside a single statement the insert's unique
// check can run before the update it depends on. The partial unique index
// still guards the race between two administrators -- the second insert
// fails with ErrDuplicate -- and callers run this inside a transaction.
func (r releaseRepo) CreateTarget(ctx context.Context, deviceID, product, artifactID, rolloutID string) (repo.Target, error) {
	if _, err := r.q.Exec(ctx,
		`update release_targets
		    set status = 'superseded', finished_at = now(), updated_at = now(),
		        result_note = 'replaced by a newer target'
		  where device_id = $1 and product = $2 and status = 'pending'`, deviceID, product); err != nil {
		return repo.Target{}, mapError(err, "supersede target")
	}
	t, err := scanTarget(r.q.QueryRow(ctx,
		`insert into release_targets (device_id, product, artifact_id, rollout_id, generation)
		 select $1, $2, $3, $4, coalesce(max(generation), 0) + 1
		   from release_targets where device_id = $1 and product = $2
		 returning `+targetColumns, deviceID, product, artifactID, rolloutID))
	if err != nil {
		return repo.Target{}, mapError(err, "create target")
	}
	return t, nil
}

func (r releaseRepo) OpenTarget(ctx context.Context, deviceID, product string) (repo.Target, error) {
	t, err := scanTarget(r.q.QueryRow(ctx,
		`select `+targetColumns+` from release_targets
		  where device_id = $1 and product = $2 and status = 'pending'`, deviceID, product))
	if err != nil {
		return repo.Target{}, mapError(err, "read open target")
	}
	return t, nil
}

func (r releaseRepo) LastSucceededTarget(ctx context.Context, deviceID, product string) (repo.Target, error) {
	t, err := scanTarget(r.q.QueryRow(ctx,
		`select `+targetColumns+` from release_targets
		  where device_id = $1 and product = $2 and status = 'succeeded'
		  order by generation desc limit 1`, deviceID, product))
	if err != nil {
		return repo.Target{}, mapError(err, "read last succeeded target")
	}
	return t, nil
}

func (r releaseRepo) TargetByID(ctx context.Context, id string) (repo.Target, error) {
	t, err := scanTarget(r.q.QueryRow(ctx, `select `+targetColumns+` from release_targets where id = $1`, id))
	if err != nil {
		return repo.Target{}, mapError(err, "read target")
	}
	return t, nil
}

func (r releaseRepo) TargetsByRollout(ctx context.Context, rolloutID string) ([]repo.Target, error) {
	return r.listTargets(ctx, `rollout_id = $1`, rolloutID)
}

func (r releaseRepo) TargetsByDevice(ctx context.Context, deviceID string) ([]repo.Target, error) {
	rows, err := r.q.Query(ctx, `select `+targetColumns+` from release_targets where device_id = $1 order by created_at desc`, deviceID)
	if err != nil {
		return nil, mapError(err, "list targets")
	}
	defer rows.Close()
	out := []repo.Target{}
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, mapError(err, "list targets")
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r releaseRepo) OpenTargets(ctx context.Context) ([]repo.Target, error) {
	return r.listTargets(ctx, `status = 'pending' and $1 = ''`, "")
}

func (r releaseRepo) TargetsSince(ctx context.Context, since time.Time) ([]repo.Target, error) {
	return r.listTargets(ctx, `created_at >= $1`, since.UTC())
}

func (r releaseRepo) listTargets(ctx context.Context, where string, arg any) ([]repo.Target, error) {
	rows, err := r.q.Query(ctx, `select `+targetColumns+` from release_targets where `+where+` order by created_at`, arg)
	if err != nil {
		return nil, mapError(err, "list targets")
	}
	defer rows.Close()
	out := []repo.Target{}
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, mapError(err, "list targets")
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r releaseRepo) FinishTarget(ctx context.Context, id string, status repo.TargetStatus, note, reportedVersion string) (repo.Target, error) {
	if status == repo.TargetPending {
		return repo.Target{}, errors.New("finish target: pending is not a terminal status")
	}
	t, err := scanTarget(r.q.QueryRow(ctx,
		`update release_targets
		    set status = $2, result_note = $3, reported_version = $4,
		        exclude_reason = case when $2 = 'excluded' then $3 else exclude_reason end,
		        finished_at = now(), updated_at = now()
		  where id = $1 and status = 'pending'
		  returning `+targetColumns, id, string(status), note, reportedVersion))
	if err == nil {
		return t, nil
	}
	if errors.Is(mapError(err, ""), repo.ErrNotFound) {
		// Either it does not exist or it is already settled; both mean the
		// caller's picture of it is stale.
		if _, lookup := r.TargetByID(ctx, id); lookup == nil {
			return repo.Target{}, fmt.Errorf("finish target: already settled: %w", repo.ErrConflict)
		}
	}
	return repo.Target{}, mapError(err, "finish target")
}

func (r releaseRepo) CancelPending(ctx context.Context, rolloutID string) (int, error) {
	tag, err := r.q.Exec(ctx,
		`update release_targets
		    set status = 'cancelled', result_note = 'cancelled before the machine started',
		        finished_at = now(), updated_at = now()
		  where rollout_id = $1 and status = 'pending'`, rolloutID)
	if err != nil {
		return 0, mapError(err, "cancel pending targets")
	}
	return int(tag.RowsAffected()), nil
}
