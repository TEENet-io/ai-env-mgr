package admincore

import (
	"reflect"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func strp(s string) *string { return &s }
func intp(n int) *int       { return &n }

func TestSetCollectEnablesAndPersistsOptions(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}

	p, err := m.SetCollect(true, strp("2026-08-01"), intp(30))
	if err != nil {
		t.Fatal(err)
	}
	if !p.CollectEnabled || p.CollectSince != "2026-08-01" || p.CollectQuietSeconds != 30 {
		t.Fatalf("SetCollect returned %+v", p)
	}
	// The change must be persisted to the shared policy object, not just returned.
	cur, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !cur.CollectEnabled || cur.CollectSince != "2026-08-01" || cur.CollectQuietSeconds != 30 {
		t.Fatalf("persisted policy = %+v", cur)
	}
}

func TestSetCollectDisableLeavesOptionsUntouched(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}

	if _, err := m.SetCollect(true, strp("2026-08-01"), intp(30)); err != nil {
		t.Fatal(err)
	}
	// disable with nil options must keep since/quiet as they were.
	p, err := m.SetCollect(false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.CollectEnabled {
		t.Fatal("collect should be disabled")
	}
	if p.CollectSince != "2026-08-01" || p.CollectQuietSeconds != 30 {
		t.Fatalf("nil options should be preserved, got since=%q quiet=%d", p.CollectSince, p.CollectQuietSeconds)
	}
}

func TestSetCollectPreservesBlockPolicy(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}

	// Establish a known block list first (DefaultPolicy already seeds several
	// domains, so this ends up as those plus example.com).
	before, err := m.MutateDomains([]string{"example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetCollect(true, nil, nil); err != nil {
		t.Fatal(err)
	}
	cur, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cur.BlockedDomains, before.BlockedDomains) {
		t.Fatalf("SetCollect changed the block list: before=%v after=%v",
			before.BlockedDomains, cur.BlockedDomains)
	}
	if !cur.CollectEnabled {
		t.Fatal("collect should be enabled")
	}
}

func TestCollectStatsCountsPerEmployeeAndTool(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	if err := m.AddUser("work1", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.AddUser("work2", "", ""); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Minute) // newest
	pfx := ossclient.DataCollectPrefix("work1")
	put := func(rel string, mt time.Time) {
		fs.objects[pfx+rel] = []byte("data")
		fs.mtimes[pfx+rel] = mt
	}
	put(".claude/projects/p/a.jsonl", t0)
	put(".claude/projects/p/b.jsonl", t1)
	put(".codex/sessions/2026/rollout-x.jsonl", t0)
	// work2 has nothing.

	stats, enabled, err := m.CollectStats()
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("collectEnabled should default to false")
	}
	if len(stats) != 2 {
		t.Fatalf("want 2 employees, got %d", len(stats))
	}

	byUser := map[string]CollectStat{}
	for _, s := range stats {
		byUser[s.User] = s
	}
	w1 := byUser["work1"]
	if w1.Claude != 2 || w1.Codex != 1 || w1.Total != 3 {
		t.Fatalf("work1 stats = %+v want claude=2 codex=1 total=3", w1)
	}
	if !w1.Latest.Equal(t1) {
		t.Fatalf("work1 latest = %v want %v", w1.Latest, t1)
	}
	w2 := byUser["work2"]
	if w2.Total != 0 || !w2.Latest.IsZero() {
		t.Fatalf("work2 stats = %+v want empty", w2)
	}
}

func TestCollectStatsReportsEnabledFlag(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	if err := m.AddUser("work1", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetCollect(true, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, enabled, err := m.CollectStats()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("CollectStats should report collectEnabled=true")
	}
}
