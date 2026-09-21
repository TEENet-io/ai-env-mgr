package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestRotationReissuesOldTokensOnActiveMachinesOnly(t *testing.T) {
	store, db, ctx := newWorkerDB(t)
	service := ops.New(store)
	now := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	// Three people with 100-day-old tokens: one on a machine that reported
	// an hour ago, one whose machine has been silent for three days, one
	// with no machine at all.
	names := []string{"active1", "silent1", "nomachine"}
	employees := map[string]repo.Employee{}
	for _, name := range names {
		e := onboard(t, ctx, service, name)
		employees[name] = e
		if _, err := store.Credentials().Store(ctx, repo.NewCredential{
			EmployeeID: e.ID, Epoch: e.AuthEpoch, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `update credential_versions set created_at = $1`, now.AddDate(0, 0, -100)); err != nil {
		t.Fatal(err)
	}
	pc1, _ := store.Devices().EnsureByHostname(ctx, "PC-1")
	pc2, _ := store.Devices().EnsureByHostname(ctx, "PC-2")
	store.Bindings().Bind(ctx, pc1.ID, employees["active1"].ID, "", "zhang")
	store.Bindings().Bind(ctx, pc2.ID, employees["silent1"].ID, "", "zhang")
	store.Devices().MarkSeen(ctx, pc1.ID, "1.2.16", now.Add(-time.Hour))
	store.Devices().MarkSeen(ctx, pc2.ID, "1.2.16", now.Add(-72*time.Hour))

	h := CredentialRotation{Store: store, Ops: service, Now: func() time.Time { return now }}
	if res, err := h.Run(ctx, repo.Task{}); err != nil || res.Note != "rotation is off" {
		t.Fatalf("off by default: %q %v", res.Note, err)
	}
	store.Settings().Set(ctx, repo.SettingRotation, []byte(`{"enabled":true,"max_age_days":90,"per_day":5,"active_within_hours":24}`), 0, "zhang")
	res, err := h.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(res.Note, "rotated 1, skipped 2: ") || !strings.Contains(res.Note, "silent1 (machine silent)") || !strings.Contains(res.Note, "nomachine (no machine)") {
		t.Fatalf("note = %q", res.Note)
	}
	active, _ := store.Employees().ByID(ctx, employees["active1"].ID)
	if active.AuthEpoch != employees["active1"].AuthEpoch+1 {
		t.Fatalf("active1 epoch = %d, want a bump", active.AuthEpoch)
	}
	silent, _ := store.Employees().ByID(ctx, employees["silent1"].ID)
	if silent.AuthEpoch != employees["silent1"].AuthEpoch {
		t.Fatal("silent1 must not be rotated: the new token could not be delivered")
	}
	events, _, _ := store.Audit().Search(ctx, repo.AuditFilter{Action: ops.ActionRotate, Limit: 10})
	if len(events) != 1 || events[0].TargetID != active.ID || events[0].ActorID != "rotation" {
		t.Fatalf("audit = %+v", events)
	}
	open, _ := store.Tasks().ListOpen(ctx, 50)
	provisions := 0
	for _, task := range open {
		if task.Kind == repo.TaskGatewayProvision && task.EmployeeID == active.ID && task.TargetEpoch != nil && *task.TargetEpoch == active.AuthEpoch {
			provisions++
		}
	}
	if provisions != 1 {
		t.Fatalf("a rotation must queue exactly one provision for the new epoch, found %d", provisions)
	}

	// The daily limit leaves the rest for tomorrow.
	store.Settings().Set(ctx, repo.SettingRotation, []byte(`{"enabled":true,"max_age_days":90,"per_day":1,"active_within_hours":24}`), 1, "zhang")
	store.Devices().MarkSeen(ctx, pc2.ID, "1.2.16", now.Add(-time.Minute))
	third := onboard(t, ctx, service, "active2")
	store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: third.ID, Epoch: third.AuthEpoch, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	db.Pool().Exec(ctx, `update credential_versions set created_at = $1 where employee_id = $2`, now.AddDate(0, 0, -100), third.ID)
	pc3, _ := store.Devices().EnsureByHostname(ctx, "PC-3")
	store.Bindings().Bind(ctx, pc3.ID, third.ID, "", "zhang")
	store.Devices().MarkSeen(ctx, pc3.ID, "1.2.16", now.Add(-time.Minute))
	if res, _ = h.Run(ctx, repo.Task{}); !strings.HasPrefix(res.Note, "rotated 1, skipped 1: nomachine") || !strings.Contains(res.Note, "1 more due (daily limit 1)") {
		t.Fatalf("limited run: %q", res.Note)
	}
	// active1's replacement has not been provisioned yet (the old credential
	// is still live); it must not be rotated a second time.
	if again, _ := store.Employees().ByID(ctx, active.ID); again.AuthEpoch != active.AuthEpoch {
		t.Fatalf("active1 rotated twice: epoch %d", again.AuthEpoch)
	}
}
