package dbstore

import (
	"errors"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestAuditIsAppendOnlyAndQueuedForTheArchive(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")

	id, err := s.Audit().Append(ctx, repo.AuditEvent{
		ActorType: repo.ActorAdmin, ActorID: "zhang",
		Action: "account.quota.set", TargetType: "employee", TargetID: e.ID,
		Before: []byte(`{"monthlyBudgetUSD":50}`),
		After:  []byte(`{"monthlyBudgetUSD":100}`),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if id == "" {
		t.Fatal("append returned no event id")
	}

	got, err := s.Audit().ByID(ctx, id)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.Action != "account.quota.set" || got.ActorID != "zhang" || got.Result != "ok" {
		t.Errorf("event = %+v, want it as recorded", got)
	}
	if got.OccurredAt.IsZero() {
		t.Error("the event has no time")
	}

	// The delivery row is created by the same statement. An event that exists
	// but was never queued is one that quietly never reaches the archive, and
	// nothing would notice.
	pending, err := s.Audit().PendingDelivery(ctx, "sls_audit", 0)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].EventID != id {
		t.Fatalf("pending = %+v, want the new event", pending)
	}

	if err := s.Audit().RecordDeliveryFailure(ctx, id, "sls_audit", "SLS returned 503"); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	stillPending, err := s.Audit().PendingDelivery(ctx, "sls_audit", 0)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(stillPending) != 1 {
		t.Error("a failed delivery stopped being pending")
	}
	if err := s.Audit().ConfirmDelivery(ctx, id, "sls_audit"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	after, err := s.Audit().PendingDelivery(ctx, "sls_audit", 0)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("%d events are still pending after confirmation", len(after))
	}

	if err := s.Audit().ConfirmDelivery(ctx, id, "nowhere"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("confirming a target nobody queued: error = %v, want ErrNotFound", err)
	}
}

func TestAuditHistoryIsNewestFirst(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	other := mustCreate(t, ctx, s, "work2")

	base := time.Now().UTC().Add(-time.Hour)
	for i, action := range []string{"account.onboard", "account.quota.set", "account.offboard"} {
		if _, err := s.Audit().Append(ctx, repo.AuditEvent{
			ActorType: repo.ActorAdmin, ActorID: "zhang", Action: action,
			TargetType: "employee", TargetID: e.ID,
			OccurredAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("append %s: %v", action, err)
		}
	}
	if _, err := s.Audit().Append(ctx, repo.AuditEvent{
		ActorType: repo.ActorWorker, Action: "gateway.revoke",
		TargetType: "employee", TargetID: other.ID,
	}); err != nil {
		t.Fatalf("append for the other employee: %v", err)
	}

	history, err := s.Audit().ByTarget(ctx, "employee", e.ID, 0)
	if err != nil {
		t.Fatalf("ByTarget: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history has %d events, want 3 (the other employee's must not be here)", len(history))
	}
	if history[0].Action != "account.offboard" || history[2].Action != "account.onboard" {
		t.Errorf("history = %q..%q, want newest first", history[0].Action, history[2].Action)
	}

	limited, err := s.Audit().ByTarget(ctx, "employee", e.ID, 2)
	if err != nil {
		t.Fatalf("ByTarget with a limit: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit 2 returned %d events", len(limited))
	}

	recent, err := s.Audit().Recent(ctx, 0)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 4 {
		t.Errorf("recent has %d events, want all 4", len(recent))
	}
}

func TestAuditRefusesEventsItCannotRecordProperly(t *testing.T) {
	s, ctx := newTestStore(t)

	if _, err := s.Audit().Append(ctx, repo.AuditEvent{ActorType: repo.ActorAdmin}); err == nil {
		t.Error("an event with no action was recorded")
	}
	// The column is jsonb and would reject it anyway; failing here names the
	// caller's mistake instead of a parser position.
	if _, err := s.Audit().Append(ctx, repo.AuditEvent{
		Action: "account.onboard", Before: []byte(`{"a":`),
	}); err == nil {
		t.Error("an event with a truncated before-value was recorded")
	}
	if _, err := s.Audit().Append(ctx, repo.AuditEvent{
		ActorType: "nobody", Action: "account.onboard",
	}); err == nil {
		t.Error("an event from an actor type the schema does not know was recorded")
	}
}

// Append belongs in the same transaction as the change it describes: an action
// that happened must not end up unrecorded, and one that was rolled back must
// not end up recorded.
func TestAuditRollsBackWithTheChangeItDescribes(t *testing.T) {
	s, ctx := newTestStore(t)

	wantErr := errors.New("the gateway said no")
	err := s.InTx(ctx, func(tx repo.Store) error {
		e, err := tx.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
		if err != nil {
			return err
		}
		if _, err := tx.Audit().Append(ctx, repo.AuditEvent{
			ActorType: repo.ActorAdmin, ActorID: "zhang", Action: "account.onboard",
			TargetType: "employee", TargetID: e.ID,
		}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("InTx error = %v", err)
	}
	recent, err := s.Audit().Recent(ctx, 0)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 0 {
		t.Errorf("the rolled-back transaction left %d audit events behind", len(recent))
	}
}
