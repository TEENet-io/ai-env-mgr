package main

import (
	"path/filepath"
	"runtime"

	"github.com/TEENet-io/ai-env-mgr/internal/codexinstructions"
	"github.com/TEENet-io/ai-env-mgr/internal/creds"
)

// codexInstructions keeps the managed block of an employee's global Codex
// instructions. The agent runs as SYSTEM, so a file it writes is granted to
// the employee explicitly, as credential delivery does.
type codexInstructions struct{}

func (codexInstructions) Sync(profileDir, user string, enabled bool) (bool, error) {
	changed, err := codexinstructions.Sync(profileDir, enabled)
	if err != nil || !changed || !enabled || runtime.GOOS != "windows" {
		return changed, err
	}
	return true, creds.GrantAccess(filepath.Join(profileDir, ".codex", "AGENTS.md"), user)
}
