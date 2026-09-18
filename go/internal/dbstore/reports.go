package dbstore

import (
	"context"
	"errors"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type reportRepo struct{ q querier }

const reportColumns = `device_id, imported_at, last_sync_at, agent_version, bound_windows_user,
	bound_user_exists, policy_etag, creds_etag, creds_applied, applocker_mode, collect_enabled,
	collect_uploaded, codex_version, codex_state, codex_restart_nonce, codex_restart_at,
	codex_restart_note, last_event, last_event_at, error_count, warning_count, report, source_etag`

func scanReport(row scanner) (repo.DeviceReport, error) {
	var r repo.DeviceReport
	err := row.Scan(&r.DeviceID, &r.ImportedAt, &r.LastSyncAt, &r.AgentVersion, &r.BoundWindowsUser,
		&r.BoundUserExists, &r.PolicyETag, &r.CredsETag, &r.CredsApplied, &r.AppLockerMode, &r.CollectEnabled,
		&r.CollectUploaded, &r.CodexVersion, &r.CodexState, &r.CodexRestartNonce, &r.CodexRestartAt,
		&r.CodexRestartNote, &r.LastEvent, &r.LastEventAt, &r.ErrorCount, &r.WarningCount, &r.Report, &r.SourceETag)
	if err != nil {
		return repo.DeviceReport{}, err
	}
	return r, nil
}

func (r reportRepo) Get(ctx context.Context, deviceID string) (repo.DeviceReport, error) {
	rep, err := scanReport(r.q.QueryRow(ctx,
		`select `+reportColumns+` from device_reported_state where device_id = $1`, deviceID))
	if err != nil {
		return repo.DeviceReport{}, mapError(err, "read machine report")
	}
	return rep, nil
}

func (r reportRepo) List(ctx context.Context) ([]repo.DeviceReport, error) {
	rows, err := r.q.Query(ctx, `select `+reportColumns+` from device_reported_state`)
	if err != nil {
		return nil, mapError(err, "list machine reports")
	}
	defer rows.Close()
	out := []repo.DeviceReport{}
	for rows.Next() {
		rep, err := scanReport(rows)
		if err != nil {
			return nil, mapError(err, "scan machine report")
		}
		out = append(out, rep)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list machine reports")
	}
	return out, nil
}

// Import writes a report unless it is unchanged or older than what is held.
//
// The guard lives in the statement, so two importers racing cannot both
// decide theirs is newer. A report with no last_sync at all is accepted only
// when nothing is held yet.
func (r reportRepo) Import(ctx context.Context, rep repo.DeviceReport) (bool, error) {
	if rep.DeviceID == "" {
		return false, errors.New("import machine report: a device is required")
	}
	if len(rep.Report) == 0 {
		rep.Report = []byte(`{}`)
	}
	tag, err := r.q.Exec(ctx,
		`insert into device_reported_state
		   (device_id, imported_at, last_sync_at, agent_version, bound_windows_user, bound_user_exists,
		    policy_etag, creds_etag, creds_applied, applocker_mode, collect_enabled, collect_uploaded,
		    codex_version, codex_state, codex_restart_nonce, codex_restart_at, codex_restart_note,
		    last_event, last_event_at, error_count, warning_count, report, source_etag)
		 values ($1, now(), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
		         $17, $18, $19, $20, $21::jsonb, $22)
		 on conflict (device_id) do update set
		   imported_at = now(), last_sync_at = excluded.last_sync_at,
		   agent_version = excluded.agent_version, bound_windows_user = excluded.bound_windows_user,
		   bound_user_exists = excluded.bound_user_exists, policy_etag = excluded.policy_etag,
		   creds_etag = excluded.creds_etag, creds_applied = excluded.creds_applied,
		   applocker_mode = excluded.applocker_mode, collect_enabled = excluded.collect_enabled,
		   collect_uploaded = excluded.collect_uploaded, codex_version = excluded.codex_version,
		   codex_state = excluded.codex_state, codex_restart_nonce = excluded.codex_restart_nonce,
		   codex_restart_at = excluded.codex_restart_at, codex_restart_note = excluded.codex_restart_note,
		   last_event = excluded.last_event, last_event_at = excluded.last_event_at,
		   error_count = excluded.error_count, warning_count = excluded.warning_count,
		   report = excluded.report, source_etag = excluded.source_etag
		 where device_reported_state.source_etag is distinct from excluded.source_etag
		   and (device_reported_state.last_sync_at is null
		        or excluded.last_sync_at is null
		        or excluded.last_sync_at >= device_reported_state.last_sync_at)`,
		rep.DeviceID, nullableTime(rep.LastSyncAt), rep.AgentVersion, rep.BoundWindowsUser, rep.BoundUserExists,
		rep.PolicyETag, rep.CredsETag, rep.CredsApplied, rep.AppLockerMode, rep.CollectEnabled, rep.CollectUploaded,
		rep.CodexVersion, rep.CodexState, rep.CodexRestartNonce, nullableTime(rep.CodexRestartAt), rep.CodexRestartNote,
		rep.LastEvent, nullableTime(rep.LastEventAt), rep.ErrorCount, rep.WarningCount, rep.Report, rep.SourceETag)
	if err != nil {
		return false, mapError(err, "import machine report")
	}
	return tag.RowsAffected() > 0, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
