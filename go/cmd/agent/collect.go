package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
)

// localFileSource finds and reads the bound employee's AI session files.
//
// It mirrors the Codex globs in collect_to_oss.py and the WorkBuddy project
// layout used by the offline collector:
//
//	<profile>/.claude/projects/**/*.jsonl
//	<profile>/.codex/sessions/**/rollout-*.jsonl
//	<profile>/.workbuddy/projects/**/*.jsonl
//	<profile>/.workbuddy-ai/projects/**/*.jsonl
//	<profile>/AppData/Roaming/WorkBuddy*/projects/**/*.jsonl
//
// plus the exact files in collectedFiles below.
//
// A missing .claude or .codex directory is normal (the employee may use only
// one tool) and is not an error.
type localFileSource struct{}

// collectedFiles are single files, relative to the profile, that are collected
// in addition to the session directories above.
//
// This is an EXACT allowlist and must stay one: it must never become a glob or
// a prefix match over .codex. That directory also holds auth.json and
// config.toml, and config.toml carries the plaintext gateway token this system
// issues to the employee. Collection uploads to the object store, so widening
// this list by pattern would publish those credentials. Add a filename here
// only after checking what that file actually contains.
var collectedFiles = []string{
	// Codex's global UI state: which threads the employee archived, with
	// their names, working directories and timestamps. Not a session, but
	// the only record that a session was archived rather than deleted.
	".codex/.codex-global-state.json",
}

func (localFileSource) Sessions(profileDir string) ([]agentcore.SessionFile, error) {
	var out []agentcore.SessionFile

	roots := []struct {
		dir   string
		match func(name string) bool
	}{
		{filepath.Join(profileDir, ".codex", "sessions"),
			func(n string) bool { return strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl") }},
		// WorkBuddy stores provider-neutral conversation records in project
		// JSONL files. Keep the roots explicit: do not scan all of .workbuddy,
		// which may contain credentials, caches, or diagnostics.
		{filepath.Join(profileDir, ".workbuddy", "projects"),
			func(n string) bool { return strings.HasSuffix(strings.ToLower(n), ".jsonl") }},
		{filepath.Join(profileDir, ".workbuddy-ai", "projects"),
			func(n string) bool { return strings.HasSuffix(strings.ToLower(n), ".jsonl") }},
		{filepath.Join(profileDir, "AppData", "Roaming", "WorkBuddy", "projects"),
			func(n string) bool { return strings.HasSuffix(strings.ToLower(n), ".jsonl") }},
		{filepath.Join(profileDir, "AppData", "Roaming", "WorkBuddy AI", "projects"),
			func(n string) bool { return strings.HasSuffix(strings.ToLower(n), ".jsonl") }},
	}

	for _, r := range roots {
		err := filepath.WalkDir(r.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable dir/entry: skip, do not abort the whole walk
			}
			// Never follow a link into another profile or outside the approved
			// WorkBuddy/Codex tree. WalkDir does not follow directory links, but
			// Open would otherwise follow file links later.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() || !r.match(d.Name()) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(profileDir, path)
			if err != nil {
				return nil
			}
			out = append(out, agentcore.SessionFile{
				Path:    path,
				Rel:     filepath.ToSlash(rel),
				ModTime: info.ModTime(),
				Size:    info.Size(),
			})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	for _, rel := range collectedFiles {
		path := filepath.Join(profileDir, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			// Absent is the normal case for an employee who does not use the
			// tool; unreadable is not worth failing the whole pass over.
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		out = append(out, agentcore.SessionFile{
			Path:    path,
			Rel:     rel,
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}
	return out, nil
}

func (localFileSource) Open(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
