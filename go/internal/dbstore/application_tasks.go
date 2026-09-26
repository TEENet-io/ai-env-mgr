package dbstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type applicationTaskRepo struct {
	q    querier
	db   *DB
	inTx bool
}

const applicationTaskColumns = `id, device_id::text, app_id, version, allow_downgrade, state, attempts,
	lease_token, lease_until, progress, last_error, created_by, created_at, updated_at, finished_at`

func scanApplicationTask(row scanner) (repo.ApplicationTask, error) {
	var t repo.ApplicationTask
	err := row.Scan(&t.ID, &t.DeviceID, &t.AppID, &t.Version, &t.AllowDowngrade, &t.State, &t.Attempts,
		&t.LeaseToken, &t.LeaseUntil, &t.Progress, &t.LastError, &t.CreatedBy,
		&t.CreatedAt, &t.UpdatedAt, &t.FinishedAt)
	return t, err
}

func (r applicationTaskRepo) Create(ctx context.Context, deviceID, appID, version, actor string, allowDowngrade bool) (repo.ApplicationTask, error) {
	create := func(q querier) (repo.ApplicationTask, error) {
		// Replacing an open request is an explicit Admin action. A running agent's
		// next lease renewal will fail and its worker will be cancelled.
		if _, err := q.Exec(ctx, `update application_tasks set state='cancelled', lease_token='',
			lease_until=null, finished_at=now(), updated_at=now()
			where device_id=$1 and app_id=$2 and state in ('pending','running')`, deviceID, appID); err != nil {
			return repo.ApplicationTask{}, mapError(err, "supersede application task")
		}
		id := fmt.Sprintf("install-%d", time.Now().UTC().UnixNano())
		t, err := scanApplicationTask(q.QueryRow(ctx, `insert into application_tasks
			(id,device_id,app_id,version,allow_downgrade,state,created_by) values ($1,$2,$3,$4,$5,'pending',$6)
			returning `+applicationTaskColumns, id, deviceID, appID, version, allowDowngrade, actor))
		return t, mapError(err, "create application task")
	}
	if r.inTx {
		return create(r.q)
	}
	var result repo.ApplicationTask
	err := r.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		result, err = create(tx)
		return err
	})
	return result, err
}

func (r applicationTaskRepo) Cancel(ctx context.Context, id, deviceID string) (repo.ApplicationTask, error) {
	t, err := scanApplicationTask(r.q.QueryRow(ctx, `update application_tasks set
		state='cancelled', lease_token='', lease_until=null, finished_at=now(), updated_at=now()
		where id=$1 and device_id=$2 and state in ('pending','running')
		returning `+applicationTaskColumns, id, deviceID))
	return t, mapError(err, "cancel application task")
}

func (r applicationTaskRepo) Claim(ctx context.Context, deviceID string, lease time.Duration) (repo.ApplicationTask, error) {
	seconds := int(lease.Seconds())
	if seconds < 30 {
		seconds = 30
	}
	t, err := scanApplicationTask(r.q.QueryRow(ctx, `update application_tasks set
		state='running', attempts=attempts+1, lease_token=gen_random_uuid()::text,
		lease_until=now()+$2 * interval '1 second', progress='queued', updated_at=now()
		where id=(select id from application_tasks where device_id=$1
			and (state='pending' or (state='running' and lease_until < now()))
			order by created_at limit 1 for update skip locked)
		returning `+applicationTaskColumns, deviceID, seconds))
	return t, mapError(err, "claim application task")
}

func (r applicationTaskRepo) Authorize(ctx context.Context, id, deviceID, token string) (repo.ApplicationTask, error) {
	t, err := scanApplicationTask(r.q.QueryRow(ctx, `select `+applicationTaskColumns+` from application_tasks
		where id=$1 and device_id=$2 and lease_token=$3 and state='running' and lease_until > now()`, id, deviceID, token))
	return t, mapError(err, "authorize application task")
}

func (r applicationTaskRepo) Renew(ctx context.Context, id, deviceID, token, progress string, lease time.Duration) (repo.ApplicationTask, error) {
	seconds := int(lease.Seconds())
	if seconds < 30 {
		seconds = 30
	}
	t, err := scanApplicationTask(r.q.QueryRow(ctx, `update application_tasks set
		lease_until=now()+$4 * interval '1 second', progress=$5, updated_at=now()
		where id=$1 and device_id=$2 and lease_token=$3 and state='running' and lease_until > now()
		returning `+applicationTaskColumns, id, deviceID, token, seconds, progress))
	return t, mapError(err, "renew application task")
}

func (r applicationTaskRepo) Finish(ctx context.Context, id, deviceID, token, state, lastError string) (repo.ApplicationTask, error) {
	if state != "succeeded" && state != "failed" && state != "cancelled" {
		return repo.ApplicationTask{}, fmt.Errorf("invalid application task terminal state %q", state)
	}
	t, err := scanApplicationTask(r.q.QueryRow(ctx, `update application_tasks set
		state=$4, last_error=$5, progress=$4, lease_token='', lease_until=null,
		finished_at=now(), updated_at=now()
		where id=$1 and device_id=$2 and lease_token=$3 and state='running' and lease_until > now()
		returning `+applicationTaskColumns, id, deviceID, token, state, lastError))
	return t, mapError(err, "finish application task")
}

func (r applicationTaskRepo) ByID(ctx context.Context, id string) (repo.ApplicationTask, error) {
	t, err := scanApplicationTask(r.q.QueryRow(ctx, `select `+applicationTaskColumns+` from application_tasks where id=$1`, id))
	return t, mapError(err, "read application task")
}

func (r applicationTaskRepo) ListRecent(ctx context.Context, limit int) ([]repo.ApplicationTask, error) {
	rows, err := r.q.Query(ctx, `select `+applicationTaskColumns+` from application_tasks order by created_at desc limit $1`, limit)
	if err != nil {
		return nil, mapError(err, "list application tasks")
	}
	defer rows.Close()
	out := []repo.ApplicationTask{}
	for rows.Next() {
		t, err := scanApplicationTask(rows)
		if err != nil {
			return nil, mapError(err, "list application tasks")
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r applicationTaskRepo) HasOpen(ctx context.Context, appID, version string) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `select exists(select 1 from application_tasks
		where app_id=$1 and version=$2 and state in ('pending','running'))`, appID, version).Scan(&exists)
	if err != nil {
		return false, mapError(err, "check open application tasks")
	}
	return exists, nil
}
