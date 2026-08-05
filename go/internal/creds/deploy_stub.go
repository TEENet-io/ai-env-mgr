//go:build !windows

package creds

import "fmt"

// StopAITools is a no-op off Windows: there is nothing to kill outside the
// target platform this agent deploys to.
func StopAITools() []string {
	return nil
}

// GrantAccess is unsupported off Windows; ACL management here is
// Windows-specific (icacls).
func GrantAccess(path, user string) error {
	return fmt.Errorf("GrantAccess: only supported on Windows")
}
