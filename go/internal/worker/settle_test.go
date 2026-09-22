package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func openCodexTarget(t *testing.T, store repo.Store, hostname, version string) (repo.Device, repo.Target) {
	t.Helper()
	ctx := t.Context()
	device, _ := store.Devices().EnsureByHostname(ctx, hostname)
	a, err := store.Releases().CreateArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: version,
		SHA256: strings.Repeat("c", 64), SizeBytes: 1, ObjectKey: "k", CreatedBy: "t"})
	if err != nil {
		a, _ = store.Releases().ArtifactByVersion(ctx, repo.ProductCodex, version)
	}
	r, _ := store.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: a.ID, Kind: repo.RolloutRelease, CreatedBy: "t"})
	target, err := store.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, a.ID, r.ID)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	return device, target
}

func TestAMatchingFreshReportSucceedsTheTarget(t *testing.T) {
	store, ctx := newWorkerStore(t)
	device, target := openCodexTarget(t, store, "PC-1", "0.42.0")
	later := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)

	if err := SettleTargets(ctx, store, device.ID, model.Status{LastSync: later, CodexVersion: "0.42.0", CodexTarget: "0.42.0", CodexTargetGeneration: target.Generation}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, _ := store.Releases().TargetByID(ctx, target.ID)
	if got.Status != repo.TargetSucceeded || got.ReportedVersion != "0.42.0" {
		t.Fatalf("after a matching report: %+v", got)
	}
}

func TestAStaleReportSettlesNothing(t *testing.T) {
	store, ctx := newWorkerStore(t)
	device, target := openCodexTarget(t, store, "PC-2", "0.42.0")
	earlier := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	// The machine already had 0.42.0 before the target existed (say, a
	// rollback to what it has). Old evidence is not evidence of this target.
	SettleTargets(ctx, store, device.ID, model.Status{LastSync: earlier, CodexVersion: "0.42.0"})
	got, _ := store.Releases().TargetByID(ctx, target.ID)
	if got.Status != repo.TargetPending {
		t.Fatalf("a report older than the target settled it: %+v", got)
	}
}

func TestAFailureForThisGenerationFailsTheTarget(t *testing.T) {
	store, ctx := newWorkerStore(t)
	device, target := openCodexTarget(t, store, "PC-3", "0.42.0")
	later := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)

	// A failure reported for an older generation is about a target that no
	// longer exists.
	SettleTargets(ctx, store, device.ID, model.Status{LastSync: later, CodexVersion: "0.41.0",
		CodexTarget: "0.42.0", CodexTargetGeneration: target.Generation - 1, CodexState: "failed",
		Errors: []string{"codex: install: exit status 2"}})
	if got, _ := store.Releases().TargetByID(ctx, target.ID); got.Status != repo.TargetPending {
		t.Fatalf("an older generation's failure settled it: %+v", got)
	}

	SettleTargets(ctx, store, device.ID, model.Status{LastSync: later, CodexVersion: "0.41.0",
		CodexTarget: "0.42.0", CodexTargetGeneration: target.Generation, CodexState: "failed",
		Errors: []string{"policy: x", "codex: install: exit status 2"}})
	got, _ := store.Releases().TargetByID(ctx, target.ID)
	if got.Status != repo.TargetFailed || got.ResultNote != "codex: install: exit status 2" {
		t.Fatalf("after a failure for this generation: %+v", got)
	}
}

func TestDeferredAndOfflineStayPending(t *testing.T) {
	store, ctx := newWorkerStore(t)
	device, target := openCodexTarget(t, store, "PC-4", "0.42.0")
	later := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	SettleTargets(ctx, store, device.ID, model.Status{LastSync: later, CodexVersion: "0.41.0",
		CodexTarget: "0.42.0", CodexTargetGeneration: target.Generation, CodexState: "deferred", CodexDeferReason: "in_use"})
	if got, _ := store.Releases().TargetByID(ctx, target.ID); got.Status != repo.TargetPending {
		t.Fatalf("deferred is not a result: %+v", got)
	}
}
