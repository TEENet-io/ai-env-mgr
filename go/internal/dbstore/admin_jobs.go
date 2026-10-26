package dbstore

import (
	"context"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type adminJobRepo struct{ q querier }

const adminJobColumns = `id, kind, version, state, step, done_bytes,
	total_bytes, last_error, started_at, ended_at, updated_at`

func scanAdminJob(row scanner) (repo.AdminJob, error) {
	var j repo.AdminJob
	if err := row.Scan(&j.ID, &j.Kind, &j.Version, &j.State, &j.Step,
		&j.DoneBytes, &j.TotalBytes, &j.LastError, &j.StartedAt,
		&j.EndedAt, &j.UpdatedAt); err != nil {
		return repo.AdminJob{}, err
	}
	return j, nil
}

func (r adminJobRepo) Create(ctx context.Context, id, kind, version string) (repo.AdminJob, error) {
	// state has no database default: a job is already running when the
	// background goroutine is created. Leaving it to PostgreSQL made every
	// publish fail with a NOT NULL violation before the upload even started.
	j, err := scanAdminJob(r.q.QueryRow(ctx, `insert into admin_jobs (id, kind, version, state, step)
		values ($1, $2, $3, 'running', '准备中') returning `+adminJobColumns, id, kind, version))
	if err != nil {
		return repo.AdminJob{}, mapError(err, "create admin job")
	}
	return j, nil
}

func (r adminJobRepo) Update(ctx context.Context, id, state, step string, doneBytes, totalBytes int64, lastError string, endedAt *time.Time) error {
	tag, err := r.q.Exec(ctx, `update admin_jobs set state=$2, step=$3,
		done_bytes=$4, total_bytes=$5, last_error=$6, ended_at=$7, updated_at=now()
		where id=$1`, id, state, step, doneBytes, totalBytes, lastError, endedAt)
	if err != nil {
		return mapError(err, "update admin job")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "update admin job")
	}
	return nil
}

func (r adminJobRepo) Latest(ctx context.Context) (repo.AdminJob, error) {
	j, err := scanAdminJob(r.q.QueryRow(ctx, `select `+adminJobColumns+`
		from admin_jobs order by updated_at desc limit 1`))
	if err != nil {
		return repo.AdminJob{}, mapError(err, "read latest admin job")
	}
	return j, nil
}

func (r adminJobRepo) ListRecent(ctx context.Context, limit int) ([]repo.AdminJob, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.q.Query(ctx, `select `+adminJobColumns+`
		from admin_jobs order by updated_at desc limit $1`, limit)
	if err != nil {
		return nil, mapError(err, "list admin jobs")
	}
	defer rows.Close()
	out := []repo.AdminJob{}
	for rows.Next() {
		j, err := scanAdminJob(rows)
		if err != nil {
			return nil, mapError(err, "scan admin job")
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list admin jobs")
	}
	return out, nil
}

func (r adminJobRepo) RecoverRunning(ctx context.Context, reason string) (int, error) {
	tag, err := r.q.Exec(ctx, `update admin_jobs set state='failed',
		last_error=$1, ended_at=now(), updated_at=now() where state='running'`, reason)
	if err != nil {
		return 0, mapError(err, "recover admin jobs")
	}
	return int(tag.RowsAffected()), nil
}
