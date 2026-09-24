package ops

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func newArtifact(product, version string) repo.NewArtifact {
	return repo.NewArtifact{Product: product, Version: version, SHA256: strings.Repeat("a", 64),
		SizeBytes: 1, ObjectKey: "k/" + version, CreatedBy: "admin"}
}

func TestRegisteringAnArtifactChangesNoTarget(t *testing.T) {
	svc, store, ctx := newService(t)
	a, err := svc.RegisterArtifact(ctx, newArtifact(repo.ProductAgent, "1.2.16"), "admin", "r1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Same bytes again is the same artifact, not an error: a retried upload
	// must land on the row it already made.
	again, err := svc.RegisterArtifact(ctx, newArtifact(repo.ProductAgent, "1.2.16"), "admin", "r2")
	if err != nil || again.ID != a.ID {
		t.Fatalf("re-register: %+v, %v", again, err)
	}
	other := newArtifact(repo.ProductAgent, "1.2.16")
	other.SHA256 = strings.Repeat("b", 64)
	if _, err := svc.RegisterArtifact(ctx, other, "admin", "r3"); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("different bytes under a used version: err = %v", err)
	}
	pol, _, _ := svc.CurrentPolicy(ctx)
	if pol.AgentUpdateVersion != "" {
		t.Fatal("registering must not aim the fleet at anything")
	}
	if tasks := openTasks(t, ctx, store); len(tasks) != 0 {
		t.Fatalf("registering queued %d task(s); it should queue none", len(tasks))
	}
}

func TestSettingTheGlobalTargetTakesTheArtifactsChecksum(t *testing.T) {
	svc, store, ctx := newService(t)
	a, _ := svc.RegisterArtifact(ctx, newArtifact(repo.ProductCodex, "0.42.0"), "admin", "r1")
	if _, err := svc.SetGlobalTarget(ctx, repo.ProductCodex, "0.99.0", "admin", "r2"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("aiming at an unregistered version: err = %v", err)
	}
	pol, err := svc.SetGlobalTarget(ctx, repo.ProductCodex, "0.42.0", "admin", "r3")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if pol.CodexVersion != "0.42.0" || pol.CodexSHA256 != a.SHA256 || pol.CodexKey != a.ObjectKey || pol.CodexRolloutPct != 100 {
		t.Fatalf("policy after set: %+v", pol)
	}
	if tasks := openTasks(t, ctx, store); len(tasks) != 1 || tasks[0].Kind != repo.TaskOSSExport {
		t.Fatalf("a policy change must queue exactly one export, got %d", len(tasks))
	}
	svc.SetArtifactStatus(ctx, a.ID, repo.ArtifactRetired, "", "admin", "r4")
	if _, err := svc.SetGlobalTarget(ctx, repo.ProductCodex, "0.42.0", "admin", "r5"); err == nil {
		t.Fatal("a retired artifact must not be aimed at anyone")
	}
	pol, _ = svc.ClearGlobalTarget(ctx, repo.ProductCodex, "admin", "r6")
	if pol.CodexVersion != "" || pol.CodexKey != "" {
		t.Fatalf("clear left %+v", pol)
	}
}

func TestARolloutAimsOnlyAtTheChosenMachines(t *testing.T) {
	svc, store, ctx := newService(t)
	a, _ := svc.RegisterArtifact(ctx, newArtifact(repo.ProductCodex, "0.42.0"), "admin", "r1")
	win1, _ := store.Devices().EnsureByHostname(ctx, "WIN-01")
	win2, _ := store.Devices().EnsureByHostname(ctx, "WIN-02")
	old, _ := store.Devices().EnsureByHostname(ctx, "WIN-OLD")
	now := time.Now()
	store.Devices().MarkSeen(ctx, win1.ID, "1.2.16", now)
	store.Devices().MarkSeen(ctx, win2.ID, "1.2.16", now)
	store.Devices().MarkSeen(ctx, old.ID, "1.2.15", now)

	_, err := svc.CreateRollout(ctx, RolloutSpec{Product: repo.ProductCodex, ArtifactID: a.ID,
		DeviceIDs: []string{win1.ID, old.ID}, Kind: repo.RolloutRelease, Actor: "admin", RequestID: "r2"})
	if err == nil || !strings.Contains(err.Error(), "WIN-OLD") {
		t.Fatalf("a machine on 1.2.15 cannot read a target; err = %v", err)
	}
	if n := len(openTasks(t, ctx, store)); n != 0 {
		t.Fatalf("a refused rollout must leave nothing behind, found %d task(s)", n)
	}

	r, err := svc.CreateRollout(ctx, RolloutSpec{Product: repo.ProductCodex, ArtifactID: a.ID,
		DeviceIDs: []string{win1.ID}, Kind: repo.RolloutRelease, Note: "试发", Actor: "admin", RequestID: "r3"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Releases().OpenTarget(ctx, win1.ID, repo.ProductCodex); err != nil {
		t.Fatalf("WIN-01 has no open target: %v", err)
	}
	if _, err := store.Releases().OpenTarget(ctx, win2.ID, repo.ProductCodex); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("WIN-02 was not chosen and must have no target, err = %v", err)
	}
	tasks := openTasks(t, ctx, store)
	if len(tasks) != 1 || tasks[0].DeviceID != win1.ID {
		t.Fatalf("want one binding export for WIN-01, got %+v", tasks)
	}
	pol, _, _ := svc.CurrentPolicy(ctx)
	if pol.CodexVersion != "" {
		t.Fatal("a targeted rollout must not touch the fleet policy")
	}

	// Pause and resume each re-export the pending machines.
	if err := svc.SetRolloutPaused(ctx, r.ID, true, "admin", "r4"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, _ := store.Releases().RolloutByID(ctx, r.ID); got.PausedAt == nil {
		t.Fatal("not paused")
	}
	if n := len(openTasks(t, ctx, store)); n != 2 {
		t.Fatalf("pausing must queue a fresh export, tasks = %d", n)
	}
}

func TestExcludeAndRetryAreExplicitAndRecorded(t *testing.T) {
	svc, store, ctx := newService(t)
	a, _ := svc.RegisterArtifact(ctx, newArtifact(repo.ProductAgent, "1.2.17"), "admin", "r1")
	win1, _ := store.Devices().EnsureByHostname(ctx, "WIN-01")
	store.Devices().MarkSeen(ctx, win1.ID, "1.2.16", time.Now())
	r, _ := svc.CreateRollout(ctx, RolloutSpec{Product: repo.ProductAgent, ArtifactID: a.ID,
		DeviceIDs: []string{win1.ID}, Kind: repo.RolloutRelease, Actor: "admin", RequestID: "r2"})
	target, _ := store.Releases().OpenTarget(ctx, win1.ID, repo.ProductAgent)

	if err := svc.ExcludeTarget(ctx, target.ID, "", "admin", "r3"); err == nil {
		t.Fatal("excluding needs a reason")
	}
	if err := svc.ExcludeTarget(ctx, target.ID, "机器已停用", "admin", "r4"); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	got, _ := store.Releases().TargetByID(ctx, target.ID)
	if got.Status != repo.TargetExcluded || got.ExcludeReason != "机器已停用" {
		t.Fatalf("after exclude: %+v", got)
	}

	// Retry is only for a failed target, and makes a new generation.
	if _, err := svc.RetryTarget(ctx, target.ID, "admin", "r5"); err == nil {
		t.Fatal("an excluded target is not retried")
	}
	t2, _ := store.Releases().CreateTarget(ctx, win1.ID, repo.ProductAgent, a.ID, r.ID)
	store.Releases().FinishTarget(ctx, t2.ID, repo.TargetFailed, "checksum mismatch", "")
	t3, err := svc.RetryTarget(ctx, t2.ID, "admin", "r6")
	if err != nil || t3.Generation != t2.Generation+1 || t3.RolloutID != r.ID {
		t.Fatalf("retry: %+v, %v", t3, err)
	}
	events, _ := store.Audit().ByTarget(ctx, "device", win1.ID, 10)
	var actions []string
	for _, ev := range events {
		actions = append(actions, ev.Action)
	}
	for _, want := range []string{ActionTargetExclude, ActionTargetRetry} {
		found := false
		for _, a := range actions {
			found = found || a == want
		}
		if !found {
			t.Fatalf("audit has %v, missing %s", actions, want)
		}
	}
}

func TestAgentCanTakeTargetsIsWhatTheRolloutChecks(t *testing.T) {
	if !model.AgentCanTakeTargets("1.2.16") {
		t.Fatal("the floor moved; update the rollout check and this test together")
	}
}

func TestFollowGlobalLetsGoOfTheMachinesOwnVersion(t *testing.T) {
	svc, store, ctx := newService(t)
	a, _ := svc.RegisterArtifact(ctx, newArtifact(repo.ProductAgent, "1.3.1"), "admin", "r1")
	win, _ := store.Devices().EnsureByHostname(ctx, "WIN-01")
	store.Devices().MarkSeen(ctx, win.ID, "1.3.0", time.Now())

	// Pinned, installed, and the rollout over: the machine is held there.
	if _, err := svc.CreateRollout(ctx, RolloutSpec{Product: repo.ProductAgent, ArtifactID: a.ID,
		DeviceIDs: []string{win.ID}, Kind: repo.RolloutRelease, Actor: "admin", RequestID: "r2"}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	open, _ := store.Releases().OpenTarget(ctx, win.ID, repo.ProductAgent)
	store.Releases().FinishTarget(ctx, open.ID, repo.TargetSucceeded, "", "1.3.1")
	if _, err := store.Releases().LastSucceededTarget(ctx, win.ID, repo.ProductAgent); err != nil {
		t.Fatalf("a finished pin should still hold the machine: %v", err)
	}
	if err := svc.FollowGlobal(ctx, win.ID, repo.ProductAgent, "admin", "r3"); err != nil {
		t.Fatalf("follow global: %v", err)
	}
	if _, err := store.Releases().LastSucceededTarget(ctx, win.ID, repo.ProductAgent); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("after 跟随全局 nothing may hold the machine, err = %v", err)
	}

	// A pin not yet installed is cancelled, not left pending.
	if _, err := svc.CreateRollout(ctx, RolloutSpec{Product: repo.ProductAgent, ArtifactID: a.ID,
		DeviceIDs: []string{win.ID}, Kind: repo.RolloutRelease, Actor: "admin", RequestID: "r4"}); err != nil {
		t.Fatalf("pin again: %v", err)
	}
	if err := svc.FollowGlobal(ctx, win.ID, repo.ProductAgent, "admin", "r5"); err != nil {
		t.Fatalf("follow global: %v", err)
	}
	if _, err := store.Releases().OpenTarget(ctx, win.ID, repo.ProductAgent); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("the open pin must be cancelled, err = %v", err)
	}
}

func TestRevokingATokenLetsTheMachineEnrolAgain(t *testing.T) {
	svc, store, ctx := newService(t)
	win, _ := store.Devices().EnsureByHostname(ctx, "WIN-01")
	if _, err := store.DeviceTokens().Issue(ctx, win.ID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := svc.RevokeDeviceToken(ctx, "WIN-01", "admin", "r1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, _ := store.Devices().ByID(ctx, win.ID)
	if got.ReenrolAllowedUntil == nil || !got.ReenrolAllowedUntil.After(time.Now()) {
		t.Fatal("revoking must open an enrolment window, or a machine the console knows is locked out")
	}
}
