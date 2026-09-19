package dbstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestAVersionNamesOneSetOfBytes(t *testing.T) {
	s, ctx := newTestStore(t)
	a, err := s.Releases().CreateArtifact(ctx, repo.NewArtifact{
		Product: repo.ProductCodex, Version: "0.42.0", SHA256: strings.Repeat("a", 64),
		SizeBytes: 700 << 20, ObjectKey: "agent_workdir/_codex/codex-setup-0.42.0.exe", CreatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Status != repo.ArtifactCandidate {
		t.Fatalf("a new artifact is %q, want candidate", a.Status)
	}
	_, err = s.Releases().CreateArtifact(ctx, repo.NewArtifact{
		Product: repo.ProductCodex, Version: "0.42.0", SHA256: strings.Repeat("b", 64),
		SizeBytes: 1, ObjectKey: "x", CreatedBy: "admin",
	})
	if !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("second package under the same version: err = %v, want ErrDuplicate", err)
	}
	// The same version of the other product is a different thing.
	if _, err := s.Releases().CreateArtifact(ctx, repo.NewArtifact{
		Product: repo.ProductAgent, Version: "0.42.0", SHA256: strings.Repeat("c", 64),
		SizeBytes: 1, ObjectKey: "y", CreatedBy: "admin",
	}); err != nil {
		t.Fatalf("agent 0.42.0 beside codex 0.42.0: %v", err)
	}
	got, err := s.Releases().ArtifactByVersion(ctx, repo.ProductCodex, "0.42.0")
	if err != nil || got.ID != a.ID {
		t.Fatalf("ByVersion = %+v, %v", got, err)
	}
	accepted, err := s.Releases().SetArtifactStatus(ctx, a.ID, repo.ArtifactAccepted, "tested on WIN-TEST-01", "admin")
	if err != nil || accepted.AcceptedAt == nil || accepted.AcceptedBy != "admin" || accepted.AcceptanceNote != "tested on WIN-TEST-01" {
		t.Fatalf("accept: %+v, %v", accepted, err)
	}
	list, err := s.Releases().ListArtifacts(ctx, repo.ProductCodex)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListArtifacts(codex) = %d, %v", len(list), err)
	}
}

func TestOneOpenTargetPerDeviceAndGenerationsClimb(t *testing.T) {
	s, ctx := newTestStore(t)
	device, _ := s.Devices().EnsureByHostname(ctx, "WIN-01")
	old := mustArtifact(t, s, repo.ProductCodex, "0.41.0")
	next := mustArtifact(t, s, repo.ProductCodex, "0.42.0")
	r1, _ := s.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: old.ID, Kind: repo.RolloutRelease, CreatedBy: "admin"})
	r2, _ := s.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: next.ID, Kind: repo.RolloutRelease, CreatedBy: "admin"})

	t1, err := s.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, old.ID, r1.ID)
	if err != nil || t1.Generation != 1 || t1.Status != repo.TargetPending {
		t.Fatalf("first target: %+v, %v", t1, err)
	}
	t2, err := s.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, next.ID, r2.ID)
	if err != nil || t2.Generation != 2 {
		t.Fatalf("second target: %+v, %v", t2, err)
	}
	was, _ := s.Releases().TargetByID(ctx, t1.ID)
	if was.Status != repo.TargetSuperseded || was.FinishedAt == nil {
		t.Fatalf("the first target should be superseded, is %q", was.Status)
	}
	open, err := s.Releases().OpenTarget(ctx, device.ID, repo.ProductCodex)
	if err != nil || open.ID != t2.ID {
		t.Fatalf("OpenTarget = %+v, %v", open, err)
	}
	// A receipt for the superseded generation cannot touch anything.
	if _, err := s.Releases().FinishTarget(ctx, t1.ID, repo.TargetSucceeded, "", "0.41.0"); !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("finishing a superseded target: err = %v, want ErrConflict", err)
	}
	done, err := s.Releases().FinishTarget(ctx, t2.ID, repo.TargetSucceeded, "", "0.42.0")
	if err != nil || done.Status != repo.TargetSucceeded || done.ReportedVersion != "0.42.0" || done.FinishedAt == nil {
		t.Fatalf("finish: %+v, %v", done, err)
	}
	if _, err := s.Releases().OpenTarget(ctx, device.ID, repo.ProductCodex); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("after finishing there is no open target: err = %v", err)
	}
	// A new target after success starts generation 3, not 1.
	t3, _ := s.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, old.ID, r1.ID)
	if t3.Generation != 3 {
		t.Fatalf("generation after a finished pair = %d, want 3", t3.Generation)
	}
}

func TestCancelPendingLeavesFinishedTargetsAlone(t *testing.T) {
	s, ctx := newTestStore(t)
	a := mustArtifact(t, s, repo.ProductAgent, "1.2.16")
	r, _ := s.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductAgent, ArtifactID: a.ID, Kind: repo.RolloutRelease, CreatedBy: "admin"})
	d1, _ := s.Devices().EnsureByHostname(ctx, "WIN-01")
	d2, _ := s.Devices().EnsureByHostname(ctx, "WIN-02")
	t1, _ := s.Releases().CreateTarget(ctx, d1.ID, repo.ProductAgent, a.ID, r.ID)
	s.Releases().CreateTarget(ctx, d2.ID, repo.ProductAgent, a.ID, r.ID)
	s.Releases().FinishTarget(ctx, t1.ID, repo.TargetSucceeded, "", "1.2.16")

	n, err := s.Releases().CancelPending(ctx, r.ID)
	if err != nil || n != 1 {
		t.Fatalf("CancelPending = %d, %v; want 1", n, err)
	}
	paused, err := s.Releases().SetRolloutPaused(ctx, r.ID, true, "admin")
	if err != nil || paused.PausedAt == nil || paused.PausedBy != "admin" {
		t.Fatalf("pause: %+v, %v", paused, err)
	}
	resumed, _ := s.Releases().SetRolloutPaused(ctx, r.ID, false, "admin")
	if resumed.PausedAt != nil {
		t.Fatal("resume must clear paused_at")
	}
	targets, _ := s.Releases().TargetsByRollout(ctx, r.ID)
	if len(targets) != 2 {
		t.Fatalf("TargetsByRollout = %d rows", len(targets))
	}
}

func mustArtifact(t *testing.T, s *Store, product, version string) repo.Artifact {
	t.Helper()
	a, err := s.Releases().CreateArtifact(t.Context(), repo.NewArtifact{
		Product: product, Version: version, SHA256: strings.Repeat("0", 64),
		SizeBytes: 1, ObjectKey: "k/" + product + "/" + version, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("artifact %s %s: %v", product, version, err)
	}
	return a
}
