package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// AuditTarget is the archive the audit trail is delivered to.
const AuditTarget = "sls_audit"

// AuditSink is where an audit event is handed to whatever ships it. Today that
// is the console's own event log, which a collector tails.
type AuditSink interface {
	Audit(eventType, msg string, fields map[string]any)
}

// AuditPublish ships the audit trail to the long-term archive, and then checks
// that it got there.
//
// The two halves are deliberately separate. Writing the line hands it to the
// collector; it does not mean the line arrived. A collector that dies with a
// full disk reports nothing to anybody, and an audit trail that is quietly
// incomplete is worse than one that is visibly behind -- so written_at and
// confirmed_at are different columns, and an event that was written but never
// seen at the far end stays on a list somebody can look at.
type AuditPublish struct {
	Store repo.Store
	Sink  AuditSink
	// Archive answers "is this event id there?". Nil disables the check, which
	// leaves events written but never confirmed -- visible, and honest about
	// what is known.
	Archive AuditArchive
	// Logstore is where the audit copies live.
	Logstore string
	// SettleFor is how long to give the collector before asking. Checking
	// immediately would report every event as missing.
	SettleFor time.Duration
	// Batch bounds one pass.
	Batch int
}

// AuditArchive is the part of the log service this needs: one query, asking
// whether particular event ids are present.
type AuditArchive interface {
	Contains(ctx context.Context, logstore string, eventIDs []string, from, to time.Time) (map[string]bool, error)
}

// Run publishes what has not been published and confirms what has settled.
func (h AuditPublish) Run(ctx context.Context, _ repo.Task) (Result, error) {
	batch := h.Batch
	if batch <= 0 {
		batch = 200
	}
	settle := h.SettleFor
	if settle <= 0 {
		settle = 5 * time.Minute
	}

	pending, err := h.Store.Audit().PendingDelivery(ctx, AuditTarget, batch)
	if err != nil {
		return Result{}, err
	}
	written := 0
	for _, event := range pending {
		h.Sink.Audit(event.Action, event.Action, map[string]any{
			// The id is the whole point: it is the same id as the row in the
			// database, which is what lets the two be reconciled line by line.
			"event_id":    event.EventID,
			"occurred_at": event.OccurredAt.UTC().Format(time.RFC3339Nano),
			"actor_type":  event.ActorType,
			"actor":       event.ActorID,
			"target_type": event.TargetType,
			"target":      event.TargetID,
			"result":      event.Result,
			"request_id":  event.RequestID,
			"before":      rawJSON(event.Before),
			"after":       rawJSON(event.After),
			"detail":      rawJSON(event.Detail),
		})
		if err := h.Store.Audit().MarkDeliveryWritten(ctx, event.EventID, AuditTarget); err != nil {
			return Result{}, err
		}
		written++
	}

	confirmed, missing, err := h.confirm(ctx, settle, batch)
	if err != nil {
		return Result{Note: fmt.Sprintf("wrote %d", written)}, err
	}

	note := fmt.Sprintf("wrote %d, confirmed %d", written, confirmed)
	if missing > 0 {
		// Not an error: the collector may simply be behind. It stays on the
		// list, and the list is what somebody looks at.
		note += fmt.Sprintf(", %d written but still not in the archive", missing)
	}
	return Result{Note: note}, nil
}

// confirm asks the archive about events that were written long enough ago to
// have arrived.
func (h AuditPublish) confirm(ctx context.Context, settle time.Duration, batch int) (confirmed, missing int, err error) {
	if h.Archive == nil {
		return 0, 0, nil
	}
	cutoff := time.Now().UTC().Add(-settle)
	events, err := h.Store.Audit().AwaitingConfirmation(ctx, AuditTarget, cutoff, batch)
	if err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, nil
	}

	ids := make([]string, 0, len(events))
	from, to := events[0].OccurredAt, events[0].OccurredAt
	for _, event := range events {
		ids = append(ids, event.EventID)
		if event.OccurredAt.Before(from) {
			from = event.OccurredAt
		}
		if event.OccurredAt.After(to) {
			to = event.OccurredAt
		}
	}
	// A margin either side: the archive indexes by receive time, which is not
	// the time the event happened.
	present, err := h.Archive.Contains(ctx, h.Logstore, ids, from.Add(-time.Hour), to.Add(time.Hour))
	if err != nil {
		return 0, 0, ClassError("archive_query", err)
	}
	for _, event := range events {
		if !present[event.EventID] {
			missing++
			if err := h.Store.Audit().RecordDeliveryFailure(ctx, event.EventID, AuditTarget,
				"written but not found in the archive yet"); err != nil {
				return confirmed, missing, err
			}
			continue
		}
		if err := h.Store.Audit().ConfirmDelivery(ctx, event.EventID, AuditTarget); err != nil {
			return confirmed, missing, err
		}
		confirmed++
	}
	return confirmed, missing, nil
}

// rawJSON keeps a stored JSON value readable in the log line instead of
// turning it into a quoted string of JSON.
func rawJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	return v
}
