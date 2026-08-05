package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/TEENet-io/airlock/internal/agentcore"
)

// localFileSource finds and reads the bound employee's AI session files.
//
// It mirrors the two globs in collect_to_oss.py:
//
//	<profile>/.claude/projects/**/*.jsonl
//	<profile>/.codex/sessions/**/rollout-*.jsonl
//
// A missing .claude or .codex directory is normal (the employee may use only
// one tool) and is not an error.
type localFileSource struct{}

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
	return out, nil
}

func (localFileSource) Open(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
