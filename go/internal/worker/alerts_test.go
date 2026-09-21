package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type fakeUsers struct{ users []litellm.User }

func (f *fakeUsers) ListUsers(context.Context) ([]litellm.User, error) { return f.users, nil }

func openOf(t *testing.T, ctx context.Context, store repo.Store, kind string) []repo.Alert {
	t.Helper()
	open, err := store.Alerts().ListOpen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []repo.Alert
	for _, a := range open {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func TestAlertEvalOpensAndClosesByRule(t *testing.T) {
	store, ctx := newWorkerStore(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	employee, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	if err != nil {
		t.Fatal(err)
	}
	pc1, _ := store.Devices().EnsureByHostname(ctx, "PC-1")
	pc2, _ := store.Devices().EnsureByHostname(ctx, "PC-2")
	spare, _ := store.Devices().EnsureByHostname(ctx, "SPARE")
	for _, d := range []repo.Device{pc1, pc2} {
		if _, err := store.Bindings().Bind(ctx, d.ID, employee.ID, "", "zhang"); err != nil {
			t.Fatal(err)
		}
	}
	// PC-1 went quiet 30 hours ago; PC-2 reported an hour ago; the spare is
	// unbound and silent for a week, which is nobody's problem.
	store.Devices().MarkSeen(ctx, pc1.ID, "1.2.16", now.Add(-30*time.Hour))
	store.Devices().MarkSeen(ctx, pc2.ID, "1.2.16", now.Add(-time.Hour))
	store.Devices().MarkSeen(ctx, spare.ID, "1.2.16", now.Add(-7*24*time.Hour))

	// A rollout that failed on PC-2, then was retried and succeeded on a new
	// generation; and one still failed on PC-1.
	art, err := store.Releases().CreateArtifact(ctx, repo.NewArtifact{
		Product: repo.ProductCodex, Version: "0.42.0", SHA256: strings.Repeat("a", 64), SizeBytes: 1, ObjectKey: "k", CreatedBy: "zhang"})
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := store.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: art.ID, Kind: repo.RolloutRelease, CreatedBy: "zhang"})
	if err != nil {
		t.Fatal(err)
	}
	target := func(device repo.Device, status repo.TargetStatus, note string) repo.Target {
		t.Helper()
		tg, err := store.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, art.ID, rollout.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Releases().FinishTarget(ctx, tg.ID, status, note, ""); err != nil {
			t.Fatal(err)
		}
		return tg
	}
	t1 := target(pc1, repo.TargetFailed, "codex: install: exit status 2")
	target(pc2, repo.TargetFailed, "disk full")
	target(pc2, repo.TargetSucceeded, "")

	budget := 100.0
	gw := &fakeUsers{users: []litellm.User{
		{UserID: "emp-work1", Spend: 85, MaxBudget: &budget},
		{UserID: "emp-work9", Spend: 10, MaxBudget: &budget},  // under the line
		{UserID: "sk-master", Spend: 900, MaxBudget: &budget}, // not an employee
	}}
	h := AlertEval{Store: store, Gateway: gw, Now: func() time.Time { return now }}
	res, err := h.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(res.Note, "opened 3, resolved 0") {
		open, _ := store.Alerts().ListOpen(ctx)
		t.Fatalf("note = %q; open = %+v", res.Note, open)
	}
	offline := openOf(t, ctx, store, repo.AlertMachineOffline)
	if len(offline) != 1 || offline[0].SubjectID != pc1.ID || !strings.Contains(offline[0].Title, "PC-1 已 30 小时未上报") {
		t.Fatalf("offline = %+v", offline)
	}
	failed := openOf(t, ctx, store, repo.AlertRolloutFailed)
	if len(failed) != 1 || failed[0].SubjectID != t1.ID || !strings.Contains(failed[0].Title, "PC-1 安装 codex 0.42.0 失败") {
		t.Fatalf("rollout failed = %+v", failed)
	}
	over := openOf(t, ctx, store, repo.AlertBudget)
	if len(over) != 1 || over[0].SubjectID != employee.ID || over[0].Severity != repo.SeverityWarn || !strings.Contains(over[0].Title, "85%") {
		t.Fatalf("budget = %+v", over)
	}

	// Running again changes nothing.
	if res, _ = h.Run(ctx, repo.Task{}); !strings.HasPrefix(res.Note, "opened 0, resolved 0") {
		t.Fatalf("second run: %q", res.Note)
	}

	// PC-1 reports, its failed target is retried, and work1 blows through
	// the budget: the first two close, the warning is replaced by a crit.
	store.Devices().MarkSeen(ctx, pc1.ID, "1.2.16", now.Add(-time.Minute))
	if _, err := store.Releases().CreateTarget(ctx, pc1.ID, repo.ProductCodex, art.ID, rollout.ID); err != nil {
		t.Fatal(err)
	}
	gw.users[0].Spend = 105
	if res, _ = h.Run(ctx, repo.Task{}); !strings.HasPrefix(res.Note, "opened 1, resolved 3") {
		t.Fatalf("third run: %q", res.Note)
	}
	if len(openOf(t, ctx, store, repo.AlertMachineOffline)) != 0 || len(openOf(t, ctx, store, repo.AlertRolloutFailed)) != 0 {
		t.Fatal("recovered conditions are still open")
	}
	over = openOf(t, ctx, store, repo.AlertBudget)
	if len(over) != 1 || over[0].Severity != repo.SeverityCrit || !strings.Contains(over[0].Title, "105%") {
		t.Fatalf("budget after overrun = %+v", over)
	}

	// Switched off, the rules neither open nor close anything.
	store.Settings().Set(ctx, repo.SettingAlerts, []byte(`{"enabled":false,"offline_after_hours":24,"budget_warn_percent":80}`), 0, "zhang")
	store.Devices().MarkSeen(ctx, pc2.ID, "1.2.16", now.Add(-72*time.Hour))
	if res, _ = h.Run(ctx, repo.Task{}); res.Note != "alerts are off" {
		t.Fatalf("disabled: %q", res.Note)
	}
	if n, _ := store.Alerts().CountOpen(ctx); n != 1 {
		t.Fatalf("disabled rules touched the table: %d open", n)
	}
}

func TestEventAlertsCloseWhenTheStateMovesOn(t *testing.T) {
	store, ctx := newWorkerStore(t)
	employee, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	cred, _ := store.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: employee.ID, Epoch: 1, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	grant, _ := store.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: employee.ID, Epoch: 1, ExternalUser: "emp-work1", KeyAlias: "emp-work1-e1", CredentialID: cred.ID})
	store.Grants().RecordActual(ctx, grant.ID, repo.ActualMissing, "")
	grant, _ = store.Grants().ByKeyAlias(ctx, "", "emp-work1-e1")
	if err := OpenDriftAlert(ctx, store, grant, repo.ActualMissing); err != nil {
		t.Fatal(err)
	}
	task, _, _ := store.Tasks().Enqueue(ctx, repo.NewTask{Kind: repo.TaskGatewayProvision, IdempotencyKey: "p1", MaxAttempts: 1, EmployeeID: employee.ID})
	claimed, _ := store.Tasks().Claim(ctx, "w", []string{repo.TaskGatewayProvision}, time.Minute)
	store.Tasks().FailPermanently(ctx, claimed.ID, "w", "gateway_rejected", "400 bad alias", "")
	if err := OpenTaskFailedAlert(ctx, store, claimed, nil); err != nil {
		t.Fatal(err)
	}
	h := AlertEval{Store: store}
	if res, err := h.Run(ctx, repo.Task{}); err != nil || !strings.HasPrefix(res.Note, "opened 0, resolved 0") {
		t.Fatalf("nothing has moved on: %q %v", res.Note, err)
	}
	if n, _ := store.Alerts().CountOpen(ctx); n != 2 {
		t.Fatalf("open = %d", n)
	}
	// The key turns up active and the task is revived by a new enqueue.
	store.Grants().RecordActual(ctx, grant.ID, repo.ActualActive, "")
	if _, created, _ := store.Tasks().Enqueue(ctx, repo.NewTask{Kind: repo.TaskGatewayProvision, IdempotencyKey: "p1", MaxAttempts: 1, EmployeeID: employee.ID}); !created {
		t.Fatal("the failed task was not revived")
	}
	_ = task
	if res, _ := h.Run(ctx, repo.Task{}); !strings.HasPrefix(res.Note, "opened 0, resolved 2") {
		t.Fatalf("after recovery: %q", res.Note)
	}
	if n, _ := store.Alerts().CountOpen(ctx); n != 0 {
		t.Fatalf("still open: %d", n)
	}
}
