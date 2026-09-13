//go:build windows

package creds

import (
	"fmt"
	"os/exec"
	"strings"
)

// taskkillNoMatchExit is what taskkill returns when its /FI filter matched no
// process. It is not a failure: the employee simply did not have the tool
// open. Treating it as one would put a red error on every routine token
// rotation for anybody who was not using Codex at that moment.
const taskkillNoMatchExit = 128

// StopAIToolsFor force-kills one user's AI tool processes and reports how
// many it ended.
//
// Scoped to the bound employee's session by /FI "USERNAME eq <user>" -- see
// stopCodexArgs for why a machine-wide kill is not acceptable on a
// multi-session desktop.
//
// "Nothing was running" comes back as (0, nil), so the caller can tell an
// empty session apart from a taskkill that actually failed.
func StopAIToolsFor(user string) (int, error) {
	// Checked before anything runs: a name that is not a name must not reach
	// the filter at all, and half the executables killed under a bad filter
	// would be worse than none.
	commands, err := stopCodexArgs(user)
	if err != nil {
		return 0, err
	}
	killed := 0
	var failed []string
	for _, args := range commands {
		proc := imageName(args)
		out, err := exec.Command("taskkill", args...).CombinedOutput()
		text := string(out)
		killed += taskkillKilled(text)
		if err == nil {
			continue
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == taskkillNoMatchExit {
			continue
		}
		if taskkillNoMatch(text) {
			continue
		}
		failed = append(failed, fmt.Sprintf("%s: %v (%s)", proc, err, strings.TrimSpace(text)))
	}
	if len(failed) > 0 {
		return killed, fmt.Errorf("taskkill: %s", strings.Join(failed, "; "))
	}
	return killed, nil
}

// GrantAccess fixes the ACL on path (recursively) so user can read and write
// it. The agent runs as SYSTEM, so files it writes are not automatically
// accessible to the logged-in user without this.
func GrantAccess(path, user string) error {
	cmd := exec.Command("icacls", path, "/grant", user+":(OI)(CI)F", "/T", "/C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls grant access to %q for %q: %w (%s)", path, user, err, strings.TrimSpace(string(out)))
	}
	return nil
}
