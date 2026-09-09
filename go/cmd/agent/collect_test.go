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
	mk(".codex/sessions/2026/08/notes.txt", "ignore\n")   // wrong extension
	mk(".codex/sessions/2026/08/other.jsonl", "ignore\n") // not rollout-*
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

func TestLocalFileSourceCollectsTheCodexGlobalState(t *testing.T) {
	// The global state file is not a session; it records which threads the
	// employee archived, which is context the session files alone do not carry.
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
	mk(".codex/.codex-global-state.json", `{"archivedThreads":[]}`)

	files, err := localFileSource{}.Sessions(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Rel != ".codex/.codex-global-state.json" {
		t.Fatalf("global state not collected: %+v", files)
	}
	if files[0].Size == 0 || files[0].ModTime.IsZero() {
		t.Errorf("size/mtime not populated: %+v", files[0])
	}
}

func TestLocalFileSourceNeverCollectsCodexSecrets(t *testing.T) {
	// .codex holds auth.json and config.toml, and config.toml carries the
	// gateway token this system hands out. Collection uploads to OSS, so the
	// single-file list must stay an exact allowlist and must never widen into
	// a pattern over .codex. This test is the tripwire for that.
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
	mk(".codex/.codex-global-state.json", `{"archivedThreads":[]}`)
	mk(".codex/auth.json", `{"tokens":{"access_token":"secret"}}`)
	mk(".codex/config.toml", "experimental_bearer_token = \"sk-secret\"\n")
	mk(".codex/.codex-global-state.json.bak", `{"archivedThreads":[]}`)
	mk(".codex/history.jsonl", "{}\n")

	files, err := localFileSource{}.Sessions(profile)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Rel != ".codex/.codex-global-state.json" {
			t.Errorf("collected something other than the allowlisted file: %s", f.Rel)
		}
	}
	if len(files) != 1 {
		t.Fatalf("expected exactly the one allowlisted file, got %+v", files)
	}
}

func TestLocalFileSourceMissingGlobalStateIsNotAnError(t *testing.T) {
	profile := t.TempDir()
	files, err := localFileSource{}.Sessions(profile)
	if err != nil {
		t.Fatalf("a profile with no .codex at all must not error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected nothing, got %+v", files)
	}
}
