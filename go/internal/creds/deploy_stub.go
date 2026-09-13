//go:build !windows

package creds

import "fmt"

// StopAIToolsFor is a no-op off Windows: there is nothing to kill outside the
// target platform this agent deploys to. It reports zero rather than an error
// so the sync loop reads the same on a developer's machine as on a desktop
// where nobody had Codex open.
//
// The name is still validated. Nothing here would act on a bad one, but the
// contract an agent sees must not depend on which platform it was built for,
// and this is the half of it that can be exercised by a test that actually
// runs.
func StopAIToolsFor(user string) (int, error) {
	if _, err := stopCodexArgs(user); err != nil {
		return 0, err
	}
	return 0, nil
}

// GrantAccess is unsupported off Windows; ACL management here is
// Windows-specific (icacls).
func GrantAccess(path, user string) error {
	return fmt.Errorf("GrantAccess: only supported on Windows")
}
