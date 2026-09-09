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
// It mirrors the two globs in collect_to_oss.py:
//
//	<profile>/.claude/projects/**/*.jsonl
//	<profile>/.codex/sessions/**/rollout-*.jsonl
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
		{filepath.Join(profileDir, ".claude", "projects"),
			func(n string) bool { return strings.HasSuffix(n, ".jsonl") }},
		{filepath.Join(profileDir, ".codex", "sessions"),
			func(n string) bool { return strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl") }},
	}

	for _, r := range roots {
		err := filepath.WalkDir(r.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable dir/entry: skip, do not abort the whole walk
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
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			// Absent is the normal case for an employee who does not use the
			// tool; unreadable is not worth failing the whole pass over.
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
