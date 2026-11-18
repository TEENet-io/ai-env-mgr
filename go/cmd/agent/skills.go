package main

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/managedskills"
)

func newManagedSkillsManager() *managedskills.Manager {
	return &managedskills.Manager{
		Root:     filepath.Join(filepath.Dir(stateDir()), "OpenAI", "Codex"),
		StateDir: stateDir(), Protect: managedskills.Protect, ProtectState: managedskills.ProtectPrivate,
		Refresh: func(user, generation string) (bool, error) {
			if user == "." || user == ".." || strings.ContainsAny(user, `/\:`) {
				return false, errors.New("invalid local skill refresh user")
			}
			ok, err := managedskills.DesktopRefreshed(filepath.Join(usersRoot(), user), user, generation)
			if err != nil || ok {
				return ok, err
			}
			running, err := codexRunningFor(user)
			return !running && err == nil, err
		},
	}
}
