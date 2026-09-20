package adminweb

import (
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestReportByVersionCountsMachinesAndTimesSuccess(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	at := func(m time.Duration) *time.Time { x := now.Add(m); return &x }
	artifacts := map[string]repo.Artifact{
		"a": {ID: "a", Product: "codex", Version: "0.42.0"},
		"b": {ID: "b", Product: "codex", Version: "0.43.0"},
	}
	targets := []repo.Target{
		{DeviceID: "d1", ArtifactID: "a", Generation: 1, Status: repo.TargetSucceeded, CreatedAt: now, FinishedAt: at(10 * time.Minute)},
		{DeviceID: "d2", ArtifactID: "a", Generation: 1, Status: repo.TargetSucceeded, CreatedAt: now, FinishedAt: at(30 * time.Minute)},
		{DeviceID: "d3", ArtifactID: "a", Generation: 1, Status: repo.TargetFailed, ResultNote: "codex: install: exit status 2", CreatedAt: now},
		{DeviceID: "d3", ArtifactID: "a", Generation: 2, Status: repo.TargetFailed, ResultNote: "codex: install: exit status 2", CreatedAt: now},
		{DeviceID: "d4", ArtifactID: "a", Generation: 1, Status: repo.TargetPending, CreatedAt: now},
		{DeviceID: "d1", ArtifactID: "b", Generation: 2, Status: repo.TargetSucceeded, CreatedAt: now, FinishedAt: at(50 * time.Minute)},
	}
	rep := reportByVersion(targets, artifacts)
	if len(rep) != 2 || rep[0].Version != "0.43.0" || rep[1].Version != "0.42.0" {
		t.Fatalf("versions newest first: %+v", rep)
	}
	r42 := rep[1]
	if r42.Succeeded != 2 || r42.Failed != 1 || r42.Pending != 1 {
		t.Fatalf("0.42.0 counts: %+v", r42)
	}
	if !r42.HasMedian || r42.MedianToSuccess != 20*time.Minute {
		t.Fatalf("median = %v (the middle of 10 and 30 is 20)", r42.MedianToSuccess)
	}
	if len(r42.TopFailures) != 1 || r42.TopFailures[0].N != 1 {
		t.Fatalf("failures counted per machine, not per attempt: %+v", r42.TopFailures)
	}
}

func TestMedianDurationTakesTheMiddleValue(t *testing.T) {
	m := func(mins ...int) time.Duration {
		var d []time.Duration
		for _, x := range mins {
			d = append(d, time.Duration(x)*time.Minute)
		}
		return median(d)
	}
	if got := m(30, 10, 20); got != 20*time.Minute {
		t.Errorf("odd count: %v", got)
	}
	if got := m(40, 10, 30, 20); got != 25*time.Minute {
		t.Errorf("even count averages the two middle values: %v", got)
	}
	if got := m(7); got != 7*time.Minute {
		t.Errorf("single: %v", got)
	}
}

func TestAlwaysDeferredListsMachinesStillWaitingAfterADay(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	artifacts := map[string]repo.Artifact{"a": {ID: "a", Product: "codex", Version: "0.42.0"}}
	targets := []repo.Target{
		{DeviceID: "d1", Product: "codex", ArtifactID: "a", Generation: 1, Status: repo.TargetPending, CreatedAt: now.Add(-36 * time.Hour)},
		{DeviceID: "d2", Product: "codex", ArtifactID: "a", Generation: 1, Status: repo.TargetPending, CreatedAt: now.Add(-2 * time.Hour)},
		{DeviceID: "d3", Product: "codex", ArtifactID: "a", Generation: 1, Status: repo.TargetPending, CreatedAt: now.Add(-36 * time.Hour)},
	}
	reports := map[string]*model.Status{
		"d1": {CodexTarget: "0.42.0", CodexTargetGeneration: 1, CodexState: "deferred", CodexDeferReason: "in_use"},
		"d2": {CodexTarget: "0.42.0", CodexTargetGeneration: 1, CodexState: "deferred", CodexDeferReason: "in_use"},
		"d3": {CodexVersion: "0.41.0"}, // offline, never saw the target
	}
	got := alwaysDeferred(targets, artifacts, map[string]string{"d1": "PC-1", "d2": "PC-2", "d3": "PC-3"}, reports, now)
	if len(got) != 1 || got[0].Hostname != "PC-1" || got[0].Reason != "正在使用" {
		t.Fatalf("deferred = %+v", got)
	}
}

func TestFleetDistributionsSplitTheFleet(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	stamp := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	machines := []admincore.MachineState{
		{Status: model.Status{AgentVersion: "1.2.16", CodexVersion: "0.42.0", AppLockerMode: "enforce", LastSync: stamp(5 * time.Minute)}},
		{Status: model.Status{AgentVersion: "1.2.16", CodexVersion: "0.42.0", AppLockerMode: "enforce", LastSync: stamp(3 * time.Hour)}},
		{Status: model.Status{AgentVersion: "1.2.14", CodexVersion: "0.41.0", AppLockerMode: "", LastSync: stamp(9 * 24 * time.Hour)}},
		{Status: model.Status{AgentVersion: "1.2.14", CodexVersion: "", LastSync: ""}},
	}
	dists := fleetDistributions(machines, now)
	if len(dists) != 4 {
		t.Fatalf("%d panels", len(dists))
	}
	agent := dists[0]
	if agent.Rows[0].Label != "1.2.16" || agent.Rows[0].N != 2 || agent.Rows[0].Pct != 50 {
		t.Fatalf("agent versions: %+v", agent.Rows)
	}
	codex := dists[1]
	if len(codex.Rows) != 3 || codex.Rows[2].Label != "未安装" {
		t.Fatalf("codex versions: %+v", codex.Rows)
	}
	if dists[2].Rows[0].Label != "enforce" || dists[2].Rows[1].Label != "未管理" {
		t.Fatalf("applocker: %+v", dists[2].Rows)
	}
	silence := dists[3]
	labels := []string{}
	for _, r := range silence.Rows {
		labels = append(labels, r.Label)
	}
	if got := join(labels); got != "15 分钟内,1 天内,超过 7 天,从未" {
		t.Fatalf("silence buckets in order: %s", got)
	}
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

func TestRolloutsAndOverviewShowTheReports(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	store := s.dbm.store
	device, _ := store.Devices().EnsureByHostname(ctx, "PC-R")
	store.Devices().MarkSeen(ctx, device.ID, "1.2.16", time.Now())
	a, _ := s.dbm.ops.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.42.0",
		SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", SizeBytes: 1, ObjectKey: "k", CreatedBy: "t"}, "t", "r")
	rollout, _ := store.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: a.ID, Kind: repo.RolloutRelease, CreatedBy: "t"})
	target, _ := store.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, a.ID, rollout.ID)
	store.Releases().FinishTarget(ctx, target.ID, repo.TargetSucceeded, "", "0.42.0")

	page := dbGet(t, h, "/rollouts", cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "版本报表") || !strings.Contains(page.Body.String(), "0.42.0") {
		t.Fatalf("rollouts report: %d", page.Code)
	}
	ov := dbGet(t, h, "/overview", cookie)
	if ov.Code != 200 || !strings.Contains(ov.Body.String(), "agent 版本") || !strings.Contains(ov.Body.String(), "多久没上报") {
		t.Fatalf("overview distributions: %d", ov.Code)
	}
}
