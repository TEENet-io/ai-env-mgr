package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestLocalFileSourceFindsSessions(t *testing.T) {
	profile := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(profile, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(".claude/projects/proj/a.jsonl", "1\n")
	mk(".codex/sessions/2026/08/rollout-abc.jsonl", "2\n")
	mk(".codex/sessions/2026/08/notes.txt", "ignore\n")        // wrong extension
	mk(".codex/sessions/2026/08/other.jsonl", "ignore\n")      // not rollout-*
	mk(".claude/projects/proj/sub/b.jsonl", "3\n")

	files, err := localFileSource{}.Sessions(profile)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, f := range files {
		rels = append(rels, f.Rel)
		if f.Size == 0 {
			t.Errorf("expected non-zero size for %s", f.Rel)
		}
	}
	sort.Strings(rels)
	want := []string{
		".claude/projects/proj/a.jsonl",
		".claude/projects/proj/sub/b.jsonl",
		".codex/sessions/2026/08/rollout-abc.jsonl",
	}
	if len(rels) != len(want) {
		t.Fatalf("rels=%v want %v", rels, want)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Fatalf("rels=%v want %v", rels, want)
		}
	}
}

func TestLocalFileSourceMissingDirsAreNotErrors(t *testing.T) {
	files, err := localFileSource{}.Sessions(t.TempDir()) // empty profile
	if err != nil {
		t.Fatalf("missing .claude/.codex must not error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("want 0 files, got %d", len(files))
	}
}
