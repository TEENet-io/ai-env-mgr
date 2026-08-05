package agentcore

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSource returns a fixed set of session files with in-memory content.
type fakeSource struct {
	files   []SessionFile
	content map[string][]byte // keyed by Path
	openErr error
}

func (s *fakeSource) Sessions(profileDir string) ([]SessionFile, error) { return s.files, nil }
func (s *fakeSource) Open(path string) (io.ReadCloser, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	return io.NopCloser(strings.NewReader(string(s.content[path]))), nil
}

// sf builds a SessionFile plus its content in one call.
func (s *fakeSource) add(rel, body string, mod time.Time) {
	p := filepath.Join("/profiles/work1", filepath.FromSlash(rel))
	s.files = append(s.files, SessionFile{Path: p, Rel: rel, ModTime: mod, Size: int64(len(body))})
	if s.content == nil {
		s.content = map[string][]byte{}
	}
	s.content[p] = []byte(body)
}

func newCollector(t *testing.T, store *fakeStore, src *fakeSource, now time.Time) *Collector {
	t.Helper()
	return &Collector{
		Store:    store,
		Source:   src,
		Machine:  &fakeMachine{name: "DESKTOP-A", localUsers: []string{"work1"}, profileDir: "/profiles"},
		StateDir: t.TempDir(),
		Now:      func() time.Time { return now },
	}
}

func TestCollectFirstRunUploadsAllWithCorrectKeys(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour) // well past the debounce window
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "line1\n", old)
	src.add(".codex/sessions/2026/rollout-x.jsonl", "line2\n", old)
	store := newFakeStore()

	res := newCollector(t, store, src, now).CollectOnce("work1", 60, "")

	if res.Uploaded != 2 {
		t.Fatalf("uploaded=%d errors=%v want 2", res.Uploaded, res.Errors)
	}
	if got := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/a.jsonl"]; string(got) != "line1\n" {
		t.Errorf("uploaded content = %q want %q", got, "line1\n")
	}
	if _, ok := store.puts["agent_workdir/work1/data_collect/.codex/sessions/2026/rollout-x.jsonl"]; !ok {
		t.Errorf("codex key missing; puts=%v", keysOf(store.puts))
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCollectSkipsUnchanged(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	store := newFakeStore()
	c := newCollector(t, store, src, now)

	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("first pass uploaded=%d want 1", r.Uploaded)
	}
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 0 {
		t.Fatalf("second pass uploaded=%d want 0 (unchanged)", r.Uploaded)
	}
}

func TestCollectReuploadsModified(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	store := newFakeStore()
	c := newCollector(t, store, src, now)
	c.CollectOnce("work1", 60, "")

	// same path, larger size + newer (still older than quiet window) mtime
	src.files[0].Size = 99
	src.files[0].ModTime = now.Add(-2 * time.Minute)
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("uploaded=%d want 1 after modification", r.Uploaded)
	}

	// changing ONLY size, with mtime untouched (still older than the quiet
	// window), must also be recognised as a change: a sig comparison that
	// dropped the size field would wrongly treat this as unchanged.
	src.files[0].Size = 12345
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("uploaded=%d want 1 after size-only change", r.Uploaded)
	}
}

func TestCollectDebounceSkipsFreshFile(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", now.Add(-10*time.Second)) // within 60s quiet
	store := newFakeStore()

	if r := newCollector(t, store, src, now).CollectOnce("work1", 60, ""); r.Uploaded != 0 {
		t.Fatalf("uploaded=%d want 0 (still being written)", r.Uploaded)
	}
}

func TestCollectFailedUploadRetries(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	store := newFakeStore()
	store.putErr = errNotFound // any error: proves state was not recorded
	c := newCollector(t, store, src, now)

	if r := c.CollectOnce("work1", 60, ""); len(r.Errors) == 0 {
		t.Fatal("want an error on failed upload")
	}
	store.putErr = nil
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("uploaded=%d want 1 on retry", r.Uploaded)
	}
}

func TestCollectPrunesDeletedSourceKeepsOSSCopy(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	store := newFakeStore()
	c := newCollector(t, store, src, now)
	c.CollectOnce("work1", 60, "")

	src.files = nil // source deleted
	c.CollectOnce("work1", 60, "")

	// OSS copy remains
	if _, ok := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/a.jsonl"]; !ok {
		t.Error("uploaded object should remain after source deletion")
	}
	// state no longer tracks it -> if the same path reappears it uploads again
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("uploaded=%d want 1 after source reappears", r.Uploaded)
	}
}

func TestCollectSinceSkipsHistory(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{}
	src.add(".claude/projects/p/old.jsonl", "x\n", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	src.add(".claude/projects/p/new.jsonl", "y\n", time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC))
	// exactly on the cutoff date: the comparison is `< since`, so an equal
	// date must be kept, not skipped.
	src.add(".claude/projects/p/boundary.jsonl", "z\n", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	store := newFakeStore()

	r := newCollector(t, store, src, now).CollectOnce("work1", 60, "2026-08-01")
	if r.Uploaded != 2 {
		t.Fatalf("uploaded=%d want 2 (only files on/after 2026-08-01)", r.Uploaded)
	}
	if _, ok := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/new.jsonl"]; !ok {
		t.Error("the newer file should have been uploaded")
	}
	if _, ok := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/boundary.jsonl"]; !ok {
		t.Error("a file dated exactly on the since cutoff should have been uploaded")
	}
}

// Every other test passes quietSeconds explicitly, so a regression in the
// `quietSeconds <= 0 -> defaultQuietSeconds` fallback (collect.go) would slip
// through the rest of the suite unnoticed. This exercises that path directly.
func TestCollectDefaultQuietWhenZero(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{}
	src.add(".claude/projects/p/fresh.jsonl", "a\n", now.Add(-30*time.Second))   // inside default 60s -> skip
	src.add(".claude/projects/p/settled.jsonl", "b\n", now.Add(-90*time.Second)) // outside default 60s -> upload
	store := newFakeStore()

	r := newCollector(t, store, src, now).CollectOnce("work1", 0, "")
	if r.Uploaded != 1 {
		t.Fatalf("uploaded=%d errors=%v want 1 (default 60s quiet window)", r.Uploaded, r.Errors)
	}
	if _, ok := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/settled.jsonl"]; !ok {
		t.Error("the file older than the default 60s quiet window should have been uploaded")
	}
	if _, ok := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/fresh.jsonl"]; ok {
		t.Error("the file within the default 60s quiet window should not have been uploaded")
	}
}
