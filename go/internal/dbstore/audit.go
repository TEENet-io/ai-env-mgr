package dbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type auditRepo struct{ q querier }

const auditColumns = `event_id, occurred_at, actor_type, actor_id, action,
	target_type, target_id, before, after, detail, result, task_id, request_id`

// The same list qualified, for the join with event_deliveries: both tables
// carry event_id.
const auditColumnsQualified = `audit_events.event_id, audit_events.occurred_at,
	audit_events.actor_type, audit_events.actor_id, audit_events.action,
	audit_events.target_type, audit_events.target_id, audit_events.before,
	audit_events.after, audit_events.detail, audit_events.result,
	audit_events.task_id, audit_events.request_id`

func scanAuditEvent(row scanner) (repo.AuditEvent, error) {
	var ev repo.AuditEvent
	var taskID *string
	err := row.Scan(&ev.EventID, &ev.OccurredAt, &ev.ActorType, &ev.ActorID, &ev.Action,
		&ev.TargetType, &ev.TargetID, &ev.Before, &ev.After, &ev.Detail,
		&ev.Result, &taskID, &ev.RequestID)
	if err != nil {
		return repo.AuditEvent{}, err
	}
	ev.TaskID = derefString(taskID)
	return ev, nil
}

// Append records one event and queues it for the log archive.
//
// The delivery row is created here, in the same statement, rather than by
// whoever ships the events: an event that exists but was never queued is an
// event that quietly never reaches the archive, and nothing would notice.
func (r auditRepo) Append(ctx context.Context, ev repo.AuditEvent) (string, error) {
	if ev.Action == "" {
		return "", errors.New("record audit event: an action is required")
	}
	if ev.ActorType == "" {
		ev.ActorType = repo.ActorSystem
	}
	if ev.Result == "" {
		ev.Result = "ok"
	}
	for _, field := range []struct {
		name  string
		value []byte
	}{{"before", ev.Before}, {"after", ev.After}, {"detail", ev.Detail}} {
		if len(field.value) > 0 && !json.Valid(field.value) {
			return "", fmt.Errorf("record audit event: %s is not valid JSON", field.name)
		}
	}
	occurred := ev.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}

	var eventID string
	err := r.q.QueryRow(ctx,
		`with recorded as (
		   insert into audit_events
		     (occurred_at, actor_type, actor_id, action, target_type, target_id,
		      before, after, detail, result, task_id, request_id)
		   values ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb, $10, $11, $12)
		   returning event_id
		 ), queued as (
		   insert into event_deliveries (event_id, target)
		   select event_id, $13 from recorded
		 )
		 select event_id from recorded`,
		occurred.UTC(), ev.ActorType, ev.ActorID, ev.Action, ev.TargetType, ev.TargetID,
		nullableJSON(ev.Before), nullableJSON(ev.After), nullableJSON(ev.Detail),
		ev.Result, nullable(ev.TaskID), ev.RequestID, auditArchiveTarget).Scan(&eventID)
	if err != nil {
		return "", mapError(err, "record audit event")
	}
	return eventID, nil
}

// auditArchiveTarget is the long-term home of the audit trail. One target
// today; the column exists so a second one does not need a migration.
const auditArchiveTarget = "sls_audit"

func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (r auditRepo) ByID(ctx context.Context, eventID string) (repo.AuditEvent, error) {
	ev, err := scanAuditEvent(r.q.QueryRow(ctx,
		`select `+auditColumns+` from audit_events where event_id = $1`, eventID))
	if err != nil {
		return repo.AuditEvent{}, mapError(err, "read audit event")
	}
	return ev, nil
}

// ByTarget is one employee's or one machine's history, newest first -- the
// list the console's detail pages show.
func (r auditRepo) ByTarget(ctx context.Context, targetType, targetID string, limit int) ([]repo.AuditEvent, error) {
	return r.query(ctx, "read audit history", limit,
		`select `+auditColumns+` from audit_events
		  where target_type = $1 and target_id = $2
		  order by occurred_at desc limit $3`, targetType, targetID)
}

func (r auditRepo) Search(ctx context.Context, f repo.AuditFilter) ([]repo.AuditEvent, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var from, to *time.Time
	if !f.From.IsZero() {
		t := f.From.UTC()
		from = &t
	}
	if !f.To.IsZero() {
		t := f.To.UTC()
		to = &t
	}
	const where = ` where ($1 = '' or target_type = $1)
		    and ($2 = '' or target_id = $2)
		    and ($3 = '' or action = $3)
		    and ($4 = '' or actor_id = $4)
		    and ($5::timestamptz is null or occurred_at >= $5)
		    and ($6::timestamptz is null or occurred_at < $6)`
	rows, err := r.q.Query(ctx,
		`select `+auditColumns+`, count(*) over() from audit_events`+where+`
		  order by occurred_at desc, event_id desc
		  limit $7 offset $8`,
		f.TargetType, f.TargetID, f.Action, f.ActorID, from, to, limit, f.Offset)
	if err != nil {
		return nil, 0, mapError(err, "search audit")
	}
	defer rows.Close()
	out := []repo.AuditEvent{}
	total := 0
	for rows.Next() {
		var ev repo.AuditEvent
		var taskID *string
		if err := rows.Scan(&ev.EventID, &ev.OccurredAt, &ev.ActorType, &ev.ActorID, &ev.Action,
			&ev.TargetType, &ev.TargetID, &ev.Before, &ev.After, &ev.Detail,
			&ev.Result, &taskID, &ev.RequestID, &total); err != nil {
			return nil, 0, mapError(err, "search audit")
		}
		ev.TaskID = derefString(taskID)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, mapError(err, "search audit")
	}
	if len(out) == 0 && f.Offset > 0 {
		// Past the end: the count is still wanted for the page links.
		if err := r.q.QueryRow(ctx, `select count(*) from audit_events`+where,
			f.TargetType, f.TargetID, f.Action, f.ActorID, from, to).Scan(&total); err != nil {
			return nil, 0, mapError(err, "count audit")
		}
	}
	return out, total, nil
}

func (r auditRepo) Recent(ctx context.Context, limit int) ([]repo.AuditEvent, error) {
	return r.query(ctx, "read recent audit events", limit,
		`select `+auditColumns+` from audit_events order by occurred_at desc limit $1`)
}

func (r auditRepo) PendingDelivery(ctx context.Context, target string, limit int) ([]repo.AuditEvent, error) {
	return r.query(ctx, "list audit events awaiting delivery", limit,
		`select `+auditColumnsQualified+`
		   from audit_events
		   join event_deliveries d
		     on d.event_id = audit_events.event_id and d.target = $1
		  where d.confirmed_at is null and d.written_at is null
		  order by audit_events.occurred_at
		  limit $2`, target)
}

func (r auditRepo) CountByRequestPrefix(ctx context.Context, prefix string) (int, error) {
	var n int
	if err := r.q.QueryRow(ctx,
		`select count(*) from audit_events where left(request_id, length($1)) = $1`, prefix).Scan(&n); err != nil {
		return 0, mapError(err, "count audit events")
	}
	return n, nil
}

func (r auditRepo) MarkDeliveryWritten(ctx context.Context, eventID, target string) error {
	tag, err := r.q.Exec(ctx,
		`update event_deliveries
		    set written_at = now(), attempts = attempts + 1, last_error = ''
		  where event_id = $1 and target = $2`, eventID, target)
	if err != nil {
		return mapError(err, "record audit delivery")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "record audit delivery")
	}
	return nil
}

func (r auditRepo) AwaitingConfirmation(ctx context.Context, target string, writtenBefore time.Time, limit int) ([]repo.AuditEvent, error) {
	return r.query(ctx, "list audit events awaiting confirmation", limit,
		`select `+auditColumnsQualified+`
		   from audit_events
		   join event_deliveries d
		     on d.event_id = audit_events.event_id and d.target = $1
		  where d.confirmed_at is null and d.written_at is not null and d.written_at < $2
		  order by audit_events.occurred_at
		  limit $3`, target, writtenBefore.UTC())
}

func (r auditRepo) ConfirmDelivery(ctx context.Context, eventID, target string) error {
	tag, err := r.q.Exec(ctx,
		`update event_deliveries
		    set confirmed_at = now(),
		        written_at = coalesce(written_at, now()),
		        attempts = attempts + 1,
		        last_error = ''
		  where event_id = $1 and target = $2`, eventID, target)
	if err != nil {
		return mapError(err, "confirm audit delivery")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "confirm audit delivery")
	}
	return nil
}

func (r auditRepo) RecordDeliveryFailure(ctx context.Context, eventID, target, reason string) error {
	tag, err := r.q.Exec(ctx,
		`update event_deliveries
		    set attempts = attempts + 1, last_error = left($3, 2000)
		  where event_id = $1 and target = $2`, eventID, target, reason)
	if err != nil {
		return mapError(err, "record audit delivery failure")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "record audit delivery failure")
	}
	return nil
}

func (r auditRepo) query(ctx context.Context, what string, limit int, sql string, args ...any) ([]repo.AuditEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.q.Query(ctx, sql, append(args, limit)...)
	if err != nil {
		return nil, mapError(err, what)
	}
	defer rows.Close()
	out := []repo.AuditEvent{}
	for rows.Next() {
		ev, err := scanAuditEvent(rows)
		if err != nil {
			return nil, mapError(err, what)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, what)
	}
	return out, nil
}
