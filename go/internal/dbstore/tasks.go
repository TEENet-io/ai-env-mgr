package dbstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type taskRepo struct{ q querier }

const taskColumns = `id, kind, idempotency_key, payload, status, attempts, max_attempts,
	lease_until, lease_owner, next_run_at, last_error, target_epoch,
	employee_id, device_id, created_at, updated_at, finished_at`

// The same list qualified with the table. Claim's UPDATE ... FROM has the
// candidate row alongside, and an unqualified "id" in its RETURNING is
// ambiguous between the two.
const taskColumnsQualified = `tasks.id, tasks.kind, tasks.idempotency_key, tasks.payload,
	tasks.status, tasks.attempts, tasks.max_attempts, tasks.lease_until, tasks.lease_owner,
	tasks.next_run_at, tasks.last_error, tasks.target_epoch, tasks.employee_id,
	tasks.device_id, tasks.created_at, tasks.updated_at, tasks.finished_at`

func scanTask(row scanner) (repo.Task, error) {
	var t repo.Task
	var status string
	var leaseUntil, finishedAt *time.Time
	var leaseOwner, employeeID, deviceID *string
	var targetEpoch *int
	err := row.Scan(&t.ID, &t.Kind, &t.IdempotencyKey, &t.Payload, &status, &t.Attempts, &t.MaxAttempts,
		&leaseUntil, &leaseOwner, &t.NextRunAt, &t.LastError, &targetEpoch,
		&employeeID, &deviceID, &t.CreatedAt, &t.UpdatedAt, &finishedAt)
	if err != nil {
		return repo.Task{}, err
	}
	t.Status = repo.TaskStatus(status)
	t.LeaseUntil = leaseUntil
	t.TargetEpoch = targetEpoch
	t.FinishedAt = finishedAt
	t.LeaseOwner = derefString(leaseOwner)
	t.EmployeeID = derefString(employeeID)
	t.DeviceID = derefString(deviceID)
	return t, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// nullable turns "" into a NULL, for the optional foreign keys. An empty
// string is not a uuid and would be rejected by the column.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const defaultMaxAttempts = 10

// Enqueue adds a task, or hands back the one already queued under this
// idempotency key.
//
// The conflict is the normal path, not an error: the whole point of the key is
// that asking twice for the same change -- a retry, a double-clicked button, a
// re-run of a half-finished onboarding -- results in one piece of work.
func (r taskRepo) Enqueue(ctx context.Context, n repo.NewTask) (repo.Task, bool, error) {
	if n.Kind == "" || n.IdempotencyKey == "" {
		return repo.Task{}, false, errors.New("enqueue task: kind and idempotency key are required")
	}
	payload := n.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	maxAttempts := n.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	notBefore := n.NotBefore
	if notBefore.IsZero() {
		notBefore = time.Now().UTC()
	}

	t, err := scanTask(r.q.QueryRow(ctx,
		`insert into tasks (kind, idempotency_key, payload, max_attempts, next_run_at,
		                    target_epoch, employee_id, device_id)
		 values ($1, $2, $3::jsonb, $4, $5, $6, $7, $8)
		 on conflict (idempotency_key) do nothing
		 returning `+taskColumns,
		n.Kind, n.IdempotencyKey, payload, maxAttempts, notBefore.UTC(),
		n.TargetEpoch, nullable(n.EmployeeID), nullable(n.DeviceID)))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return repo.Task{}, false, mapError(err, "enqueue task")
	}

	existing, err := scanTask(r.q.QueryRow(ctx,
		`select `+taskColumns+` from tasks where idempotency_key = $1`, n.IdempotencyKey))
	if err != nil {
		return repo.Task{}, false, mapError(err, "read existing task")
	}
	return existing, false, nil
}

// Claim takes one runnable task and leases it to owner.
//
// Everything happens in one statement: selecting the task with FOR UPDATE SKIP
// LOCKED so that concurrent workers step past each other instead of queueing,
// marking it running, and recording the attempt. Two statements would leave a
// window in which a task is claimed but the attempt is not recorded, which is
// the row an incident needs most.
//
// Tasks whose target epoch has been passed are not returned: the employee has
// moved on -- offboarded, re-issued -- and applying old work would hand back
// credentials that were deliberately invalidated.
func (r taskRepo) Claim(ctx context.Context, owner string, kinds []string, lease time.Duration) (repo.Task, error) {
	if owner == "" {
		return repo.Task{}, errors.New("claim task: an owner is required")
	}
	if lease <= 0 {
		lease = time.Minute
	}
	if kinds == nil {
		kinds = []string{}
	}

	t, err := scanTask(r.q.QueryRow(ctx,
		`with runnable as (
		   select t.id
		     from tasks t
		     left join employees e on e.id = t.employee_id
		    where t.status in ('pending', 'retry_wait')
		      and t.next_run_at <= now()
		      and (cardinality($2::text[]) = 0 or t.kind = any($2::text[]))
		      and (t.target_epoch is null or e.id is null or t.target_epoch >= e.auth_epoch)
		    order by t.next_run_at
		      for update of t skip locked
		    limit 1
		 ), claimed as (
		   update tasks
		      set status = 'running',
		          attempts = attempts + 1,
		          lease_owner = $1,
		          lease_until = now() + $3::interval,
		          updated_at = now()
		     from runnable
		    where tasks.id = runnable.id
		    returning `+taskColumnsQualified+`
		 ), logged as (
		   insert into task_attempts (task_id, attempt, owner)
		   select id, attempts, $1 from claimed
		 )
		 select `+taskColumns+` from claimed`,
		owner, kinds, lease.String()))
	if err != nil {
		// Nothing to do is the ordinary case, not a fault: the Worker asks
		// every few seconds and mostly there is nothing.
		return repo.Task{}, mapError(err, "claim task")
	}
	return t, nil
}

func (r taskRepo) Extend(ctx context.Context, id, owner string, lease time.Duration) error {
	if lease <= 0 {
		lease = time.Minute
	}
	tag, err := r.q.Exec(ctx,
		`update tasks
		    set lease_until = now() + $3::interval, updated_at = now()
		  where id = $1 and lease_owner = $2 and status = 'running' and lease_until > now()`,
		id, owner, lease.String())
	if err != nil {
		return mapError(err, "extend task lease")
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("extend task lease: %w", repo.ErrLeaseLost)
	}
	return nil
}

func (r taskRepo) Succeed(ctx context.Context, id, owner, externalRef string) error {
	// The task update is the statement that decides the outcome; closing the
	// attempt row rides along in a CTE. Written the other way round, a missing
	// attempt row would report a lost lease for a task that was in fact closed.
	var closedID string
	err := r.q.QueryRow(ctx,
		`with closed as (
		   update tasks
		      set status = 'succeeded', finished_at = now(), lease_owner = null,
		          lease_until = null, last_error = '', updated_at = now()
		    where id = $1 and lease_owner = $2 and status = 'running' and lease_until > now()
		    returning id, attempts
		 ), logged as (
		   update task_attempts a
		      set ended_at = now(), outcome = 'succeeded', external_ref = $3
		     from closed
		    where a.task_id = closed.id and a.attempt = closed.attempts
		 )
		 select id from closed`,
		id, owner, externalRef).Scan(&closedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the lease expired and somebody else has it, or this task
			// was already closed. Either way the result is not ours to record,
			// and a blind retry could repeat work already done.
			return fmt.Errorf("complete task: %w", repo.ErrLeaseLost)
		}
		return mapError(err, "complete task")
	}
	return nil
}

// Fail records a failed attempt and decides what happens next: back to the
// queue until the attempts run out, then failed for good.
//
// Retrying for ever turns one broken task into a permanent load on whatever it
// is calling, and hides the fault in a list that never empties.
func (r taskRepo) Fail(ctx context.Context, id, owner string, retryAt time.Time, errorClass, detail, externalRef string) (repo.Task, error) {
	if retryAt.IsZero() {
		retryAt = time.Now().UTC().Add(time.Minute)
	}
	t, err := scanTask(r.q.QueryRow(ctx,
		`with closed as (
		   update tasks
		      set status = case when attempts >= max_attempts then 'failed' else 'retry_wait' end,
		          finished_at = case when attempts >= max_attempts then now() else null end,
		          next_run_at = $3,
		          last_error = left($4, 2000),
		          lease_owner = null,
		          lease_until = null,
		          updated_at = now()
		    where id = $1 and lease_owner = $2 and status = 'running' and lease_until > now()
		    returning `+taskColumns+`
		 ), logged as (
		   update task_attempts a
		      set ended_at = now(),
		          outcome = case when closed.status = 'failed' then 'failed' else 'retry' end,
		          error_class = $5, error_detail = left($4, 2000), external_ref = $6
		     from closed
		    where a.task_id = closed.id and a.attempt = closed.attempts
		 )
		 select `+taskColumns+` from closed`,
		id, owner, retryAt.UTC(), detail, errorClass, externalRef))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return repo.Task{}, fmt.Errorf("record task failure: %w", repo.ErrLeaseLost)
		}
		return repo.Task{}, mapError(err, "record task failure")
	}
	return t, nil
}

// FailPermanently closes a task without another attempt.
//
// It is the same shape as Fail, minus the arithmetic about attempts: some
// failures are facts about the request rather than about the moment, and
// trying them nine more times only delays somebody noticing.
func (r taskRepo) FailPermanently(ctx context.Context, id, owner, errorClass, detail, externalRef string) (repo.Task, error) {
	t, err := scanTask(r.q.QueryRow(ctx,
		`with closed as (
		   update tasks
		      set status = 'failed', finished_at = now(), next_run_at = now(),
		          last_error = left($3, 2000), lease_owner = null, lease_until = null,
		          updated_at = now()
		    where id = $1 and lease_owner = $2 and status = 'running' and lease_until > now()
		    returning `+taskColumns+`
		 ), logged as (
		   update task_attempts a
		      set ended_at = now(), outcome = 'failed', error_class = $4,
		          error_detail = left($3, 2000), external_ref = $5
		     from closed
		    where a.task_id = closed.id and a.attempt = closed.attempts
		 )
		 select `+taskColumns+` from closed`,
		id, owner, detail, errorClass, externalRef))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return repo.Task{}, fmt.Errorf("fail task: %w", repo.ErrLeaseLost)
		}
		return repo.Task{}, mapError(err, "fail task")
	}
	return t, nil
}

func (r taskRepo) Supersede(ctx context.Context, id, reason string) error {
	tag, err := r.q.Exec(ctx,
		`update tasks
		    set status = 'superseded', finished_at = now(), lease_owner = null,
		        lease_until = null, last_error = left($2, 2000), updated_at = now()
		  where id = $1 and status in ('pending', 'running', 'retry_wait')`,
		id, reason)
	if err != nil {
		return mapError(err, "supersede task")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "supersede task")
	}
	return nil
}

// SupersedeOpenForEmployee abandons work aimed at an epoch the employee has
// passed. Offboarding and re-issuing call it in the same transaction as the
// epoch bump, so there is no moment at which the old work is still runnable.
func (r taskRepo) SupersedeOpenForEmployee(ctx context.Context, employeeID string, belowEpoch int) (int, error) {
	tag, err := r.q.Exec(ctx,
		`update tasks
		    set status = 'superseded', finished_at = now(), lease_owner = null,
		        lease_until = null,
		        last_error = 'superseded: the employee moved to a newer epoch',
		        updated_at = now()
		  where employee_id = $1
		    and status in ('pending', 'running', 'retry_wait')
		    and target_epoch is not null and target_epoch < $2`,
		employeeID, belowEpoch)
	if err != nil {
		return 0, mapError(err, "supersede tasks")
	}
	return int(tag.RowsAffected()), nil
}

func (r taskRepo) ByID(ctx context.Context, id string) (repo.Task, error) {
	t, err := scanTask(r.q.QueryRow(ctx, `select `+taskColumns+` from tasks where id = $1`, id))
	if err != nil {
		return repo.Task{}, mapError(err, "read task")
	}
	return t, nil
}

func (r taskRepo) ListOpen(ctx context.Context, limit int) ([]repo.Task, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.q.Query(ctx,
		`select `+taskColumns+` from tasks
		  where status in ('pending', 'running', 'retry_wait')
		  order by next_run_at limit $1`, limit)
	if err != nil {
		return nil, mapError(err, "list tasks")
	}
	defer rows.Close()
	out := []repo.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, mapError(err, "scan task")
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list tasks")
	}
	return out, nil
}

func (r taskRepo) Attempts(ctx context.Context, taskID string) ([]repo.TaskAttempt, error) {
	rows, err := r.q.Query(ctx,
		`select id, task_id, attempt, owner, started_at, ended_at,
		        coalesce(outcome, ''), error_class, error_detail, external_ref
		   from task_attempts where task_id = $1 order by attempt`, taskID)
	if err != nil {
		return nil, mapError(err, "read task attempts")
	}
	defer rows.Close()
	out := []repo.TaskAttempt{}
	for rows.Next() {
		var a repo.TaskAttempt
		var ended *time.Time
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Attempt, &a.Owner, &a.StartedAt, &ended,
			&a.Outcome, &a.ErrorClass, &a.ErrorDetail, &a.ExternalRef); err != nil {
			return nil, mapError(err, "scan task attempt")
		}
		a.EndedAt = ended
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "read task attempts")
	}
	return out, nil
}

// ReleaseExpiredLeases returns tasks whose worker stopped holding them.
//
// A lease is not a lock: a process that is killed mid-task holds nothing, and
// without this the task would sit in 'running' for ever with nobody working on
// it. The attempt is closed as 'failed' with a class that says what happened,
// so it is visible rather than silently repeated.
func (r taskRepo) ReleaseExpiredLeases(ctx context.Context) (int, error) {
	var released int
	err := r.q.QueryRow(ctx,
		`with expired as (
		   update tasks
		      set status = case when attempts >= max_attempts then 'failed' else 'retry_wait' end,
		          finished_at = case when attempts >= max_attempts then now() else null end,
		          lease_owner = null, lease_until = null,
		          last_error = 'the worker stopped holding this task',
		          updated_at = now()
		    where status = 'running' and lease_until is not null and lease_until <= now()
		    returning id, attempts
		 ), logged as (
		   update task_attempts a
		      set ended_at = now(), outcome = 'failed', error_class = 'lease_expired'
		     from expired
		    where a.task_id = expired.id and a.attempt = expired.attempts and a.ended_at is null
		 )
		 select count(*) from expired`).Scan(&released)
	if err != nil {
		return 0, mapError(err, "release expired task leases")
	}
	return released, nil
}
