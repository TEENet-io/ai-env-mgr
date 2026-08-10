//go:build !windows

package main

import "errors"

// localUpdater is a no-op off Windows: the agent is a Windows service, and
// self-replacement + service restart only makes sense there. This keeps
// Linux/dev builds compiling.
type localUpdater struct{}

func (localUpdater) ApplyUpdate([]byte) error {
	return errors.New("agent self-update is only supported on Windows")
}
