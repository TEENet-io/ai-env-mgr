package dbstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type alertRepo struct{ q querier }

const alertColumns = `id, kind, fingerprint, severity, subject_type, subject_id, title, detail,
	opened_at, resolved_at, resolved_by, acked_at, acked_by, notified_at, notify_tries, notify_error`

func scanAlert(row scanner) (repo.Alert, error) {
	var a repo.Alert
	err := row.Scan(&a.ID, &a.Kind, &a.Fingerprint, &a.Severity, &a.SubjectType, &a.SubjectID, &a.Title, &a.Detail,
		&a.OpenedAt, &a.ResolvedAt, &a.ResolvedBy, &a.AckedAt, &a.AckedBy, &a.NotifiedAt, &a.NotifyTries, &a.NotifyError)
	return a, err
}

func (r alertRepo) Open(ctx context.Context, n repo.NewAlert) (repo.Alert, bool, error) {
	if n.Kind == "" || n.Fingerprint == "" || n.Title == "" {
		return repo.Alert{}, false, errors.New("open alert: kind, fingerprint and title are required")
	}
	if n.Severity == "" {
		n.Severity = repo.SeverityWarn
	}
	// The partial unique index is the lock: two evaluators opening the same
	// condition at once both try the insert and one of them gets nothing
	// back, then reads the winner's row.
	a, err := scanAlert(r.q.QueryRow(ctx,
		`insert into alerts (kind, fingerprint, severity, subject_type, subject_id, title, detail)
		 values ($1, $2, $3, $4, $5, $6, $7)
		 on conflict (fingerprint) where resolved_at is null do nothing
		 returning `+alertColumns,
		n.Kind, n.Fingerprint, n.Severity, n.SubjectType, n.SubjectID, n.Title, n.Detail))
	if err == nil {
		return a, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return repo.Alert{}, false, mapError(err, "open alert")
	}
	a, err = scanAlert(r.q.QueryRow(ctx,
		`select `+alertColumns+` from alerts where fingerprint = $1 and resolved_at is null`, n.Fingerprint))
	if err != nil {
		return repo.Alert{}, false, mapError(err, "read open alert")
	}
	return a, false, nil
}

func (r alertRepo) Resolve(ctx context.Context, fingerprint, by string) (int, error) {
	tag, err := r.q.Exec(ctx,
		`update alerts set resolved_at = now(), resolved_by = $2 where fingerprint = $1 and resolved_at is null`,
		fingerprint, by)
	if err != nil {
		return 0, mapError(err, "resolve alert")
	}
	return int(tag.RowsAffected()), nil
}

func (r alertRepo) ResolveByID(ctx context.Context, id, by string) (repo.Alert, error) {
	a, err := scanAlert(r.q.QueryRow(ctx,
		`update alerts set resolved_at = coalesce(resolved_at, now()),
		        resolved_by = case when resolved_at is null then $2 else resolved_by end
		  where id = $1 returning `+alertColumns, id, by))
	if err != nil {
		return repo.Alert{}, mapError(err, "resolve alert")
	}
	return a, nil
}

func (r alertRepo) Ack(ctx context.Context, id, by string) (repo.Alert, error) {
	a, err := scanAlert(r.q.QueryRow(ctx,
		`update alerts set acked_at = coalesce(acked_at, now()),
		        acked_by = case when acked_at is null then $2 else acked_by end
		  where id = $1 returning `+alertColumns, id, by))
	if err != nil {
		return repo.Alert{}, mapError(err, "acknowledge alert")
	}
	return a, nil
}

func (r alertRepo) ByID(ctx context.Context, id string) (repo.Alert, error) {
	a, err := scanAlert(r.q.QueryRow(ctx, `select `+alertColumns+` from alerts where id = $1`, id))
	if err != nil {
		return repo.Alert{}, mapError(err, "read alert")
	}
	return a, nil
}

func (r alertRepo) ListOpen(ctx context.Context) ([]repo.Alert, error) {
	return r.list(ctx, "list open alerts",
		`select `+alertColumns+` from alerts where resolved_at is null order by opened_at desc`)
}

func (r alertRepo) ListRecent(ctx context.Context, limit int) ([]repo.Alert, error) {
	if limit <= 0 {
		limit = 200
	}
	return r.list(ctx, "list recent alerts",
		`select `+alertColumns+` from alerts order by opened_at desc limit $1`, limit)
}

func (r alertRepo) Unnotified(ctx context.Context, maxTries, limit int) ([]repo.Alert, error) {
	if limit <= 0 {
		limit = 50
	}
	return r.list(ctx, "list unnotified alerts",
		`select `+alertColumns+` from alerts
		  where resolved_at is null and notified_at is null and notify_tries <= $1
		  order by opened_at limit $2`, maxTries, limit)
}

func (r alertRepo) MarkNotified(ctx context.Context, id string, at time.Time, errText string) error {
	var err error
	if errText == "" {
		_, err = r.q.Exec(ctx, `update alerts set notified_at = $2, notify_error = '' where id = $1`, id, at.UTC())
	} else {
		_, err = r.q.Exec(ctx, `update alerts set notify_tries = notify_tries + 1, notify_error = $2 where id = $1`, id, errText)
	}
	return mapError(err, "record notification")
}

func (r alertRepo) CountOpen(ctx context.Context) (int, error) {
	var n int
	if err := r.q.QueryRow(ctx, `select count(*) from alerts where resolved_at is null`).Scan(&n); err != nil {
		return 0, mapError(err, "count alerts")
	}
	return n, nil
}

func (r alertRepo) list(ctx context.Context, what, sql string, args ...any) ([]repo.Alert, error) {
	rows, err := r.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, what)
	}
	defer rows.Close()
	out := []repo.Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, mapError(err, what)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
