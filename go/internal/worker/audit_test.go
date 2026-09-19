package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// fakeSink is the event log the collector tails.
type fakeSink struct {
	mu     sync.Mutex
	events []map[string]any
}

func (s *fakeSink) AuditRecorded(eventID string, occurredAt time.Time, _, _ string, fields map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := map[string]any{"event_id": eventID, "occurred_at": occurredAt}
	for k, v := range fields {
		copied[k] = v
	}
	s.events = append(s.events, copied)
}

func (s *fakeSink) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, ev := range s.events {
		if id, ok := ev["event_id"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}

// fakeArchive is the far end: whatever has been told to have arrived.
type fakeArchive struct {
	arrived map[string]bool
	err     error
	queried int
}

func (a *fakeArchive) Contains(_ context.Context, _ string, ids []string, _, _ time.Time) (map[string]bool, error) {
	a.queried++
	if a.err != nil {
		return nil, a.err
	}
	out := map[string]bool{}
	for _, id := range ids {
		if a.arrived[id] {
			out[id] = true
		}
	}
	return out, nil
}

func auditing(t *testing.T) (*dbstore.Store, *fakeSink, *fakeArchive, context.Context) {
	t.Helper()
	store, ctx := newWorkerStore(t)
	return store, &fakeSink{}, &fakeArchive{arrived: map[string]bool{}}, ctx
}

func record(t *testing.T, ctx context.Context, store *dbstore.Store, action string) string {
	t.Helper()
	id, err := store.Audit().Append(ctx, repo.AuditEvent{
		ActorType: repo.ActorAdmin, ActorID: "zhang", Action: action,
		TargetType: "employee", TargetID: "work1",
		After: []byte(`{"monthlyBudgetUSD":100}`),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return id
}

func TestAuditIsWrittenOnceAndConfirmedSeparately(t *testing.T) {
	store, sink, archive, ctx := auditing(t)
	first := record(t, ctx, store, "account.onboard")
	second := record(t, ctx, store, "account.quota")

	handler := AuditPublish{
		Store: store, Sink: sink, Archive: archive,
		Logstore: "audit", SettleFor: time.Millisecond,
	}
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Both were handed to the collector, carrying the id that makes the two
	// copies reconcilable.
	if got := sink.ids(); len(got) != 2 {
		t.Fatalf("wrote %v, want both events", got)
	}
	// Written is not arrived. Nothing is confirmed until the far end says so.
	pending, err := store.Audit().PendingDelivery(ctx, AuditTarget, 0)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d events are still unwritten", len(pending))
	}

	// A second pass must not write them again: a duplicated audit trail is a
	// trail nobody can count.
	time.Sleep(5 * time.Millisecond)
	archive.arrived[first] = true
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := sink.ids(); len(got) != 2 {
		t.Errorf("the second pass wrote the events again: %v", got)
	}

	// The one the archive has is confirmed; the other stays on the list.
	waiting, err := store.Audit().AwaitingConfirmation(ctx, AuditTarget, time.Now(), 0)
	if err != nil {
		t.Fatalf("awaiting: %v", err)
	}
	if len(waiting) != 1 || waiting[0].EventID != second {
		t.Fatalf("still waiting on %d events, want only the one that has not arrived", len(waiting))
	}
}

func TestAuditIsNotCheckedBeforeItCouldHaveArrived(t *testing.T) {
	store, sink, archive, ctx := auditing(t)
	record(t, ctx, store, "account.onboard")

	// Checking immediately would report every event as missing and raise an
	// alarm about a collector that is doing its job.
	handler := AuditPublish{
		Store: store, Sink: sink, Archive: archive,
		Logstore: "audit", SettleFor: time.Hour,
	}
	result, err := handler.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if archive.queried != 0 {
		t.Error("the archive was asked about an event written a moment ago")
	}
	if result.Note == "" {
		t.Error("the pass said nothing about what it did")
	}
}

// An event written long ago that the archive still does not have is the
// failure this whole split exists to make visible.
func TestAnAuditEventThatNeverArrivesStaysVisible(t *testing.T) {
	store, sink, archive, ctx := auditing(t)
	id := record(t, ctx, store, "account.offboard")

	handler := AuditPublish{
		Store: store, Sink: sink, Archive: archive,
		Logstore: "audit", SettleFor: time.Millisecond,
	}
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	result, err := handler.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.Note == "" || !strings.Contains(result.Note, "not in the archive") {
		t.Errorf("result = %q, want it to say the event has not arrived", result.Note)
	}

	waiting, err := store.Audit().AwaitingConfirmation(ctx, AuditTarget, time.Now(), 0)
	if err != nil {
		t.Fatalf("awaiting: %v", err)
	}
	if len(waiting) != 1 || waiting[0].EventID != id {
		t.Fatalf("waiting = %d events, want the one that never arrived", len(waiting))
	}

	// It arrives late; the next pass settles it.
	archive.arrived[id] = true
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	waiting, err = store.Audit().AwaitingConfirmation(ctx, AuditTarget, time.Now(), 0)
	if err != nil {
		t.Fatalf("awaiting: %v", err)
	}
	if len(waiting) != 0 {
		t.Errorf("%d events are still waiting after the archive got them", len(waiting))
	}
}

func TestAnArchiveThatCannotBeQueriedIsNotAMissingEvent(t *testing.T) {
	store, sink, archive, ctx := auditing(t)
	id := record(t, ctx, store, "account.onboard")

	handler := AuditPublish{
		Store: store, Sink: sink, Archive: archive,
		Logstore: "audit", SettleFor: time.Millisecond,
	}
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	archive.err = errors.New("the log service is not answering")
	_, err := handler.Run(ctx, repo.Task{})
	if err == nil {
		t.Fatal("a failing archive was reported as success")
	}
	if IsPermanent(err) {
		t.Error("a log service that is down was treated as permanent")
	}
	// And it must not have been recorded as confirmed on the way past.
	waiting, err := store.Audit().AwaitingConfirmation(ctx, AuditTarget, time.Now(), 0)
	if err != nil {
		t.Fatalf("awaiting: %v", err)
	}
	if len(waiting) != 1 || waiting[0].EventID != id {
		t.Errorf("waiting = %d, want the event still unconfirmed", len(waiting))
	}
}

// Writing works with no archive configured; the events simply stay
// unconfirmed, which is an honest description of what is known.
func TestPublishingWorksWithoutAnArchiveToAsk(t *testing.T) {
	store, sink, _, ctx := auditing(t)
	record(t, ctx, store, "account.onboard")

	handler := AuditPublish{Store: store, Sink: sink, Logstore: "audit"}
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(sink.ids()) != 1 {
		t.Fatal("the event was not written")
	}
	waiting, err := store.Audit().AwaitingConfirmation(ctx, AuditTarget, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("awaiting: %v", err)
	}
	if len(waiting) != 1 {
		t.Errorf("waiting = %d, want the event written and unconfirmed", len(waiting))
	}
}
