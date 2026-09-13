//go:build !windows

package creds

import "fmt"

// StopAIToolsFor is a no-op off Windows: there is nothing to kill outside the
// target platform this agent deploys to. It reports zero rather than an error
// so the sync loop reads the same on a developer's machine as on a desktop
// where nobody had Codex open.
func StopAIToolsFor(user string) (int, error) {
	return 0, nil
}

// GrantAccess is unsupported off Windows; ACL management here is
// Windows-specific (icacls).
func GrantAccess(path, user string) error {
	return fmt.Errorf("GrantAccess: only supported on Windows")
}
