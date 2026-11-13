package main

import (
	"path/filepath"

	"github.com/TEENet-io/ai-env-mgr/internal/managedskills"
)

func newManagedSkillsManager() *managedskills.Manager {
	return &managedskills.Manager{
		Root:     filepath.Join(filepath.Dir(stateDir()), "OpenAI", "Codex"),
		StateDir: stateDir(), Protect: managedskills.Protect, ProtectState: managedskills.ProtectPrivate,
		Stop: func(user string) error { _, err := (localApplier{}).StopCodex(user); return err },
	}
}
