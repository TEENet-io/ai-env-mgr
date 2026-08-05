//go:build windows

package creds

import (
	"fmt"
	"os/exec"
	"strings"
)

// aiToolProcesses are the executables that hold AI tool credentials open
// in memory. They are killed before a credential deploy so the tool is
// forced to reload from disk on next launch.
var aiToolProcesses = []string{"ChatGPT.exe", "codex.exe", "claude.exe"}

// StopAITools force-kills the running AI tool processes and returns the
// names of the ones that were actually stopped.
//
// Why kill them at all: these tools cache credentials in memory and do not
// watch auth.json (or the Claude equivalents) for changes. Codex's own
// source is explicit that an external edit to auth.json is not picked up
// until the process does an explicit reload. So overwriting the files on
// disk while the tool keeps running has no effect until the process is
// restarted — StopAITools makes sure the next launch reads what was just
// deployed instead of what it already had cached.
func StopAITools() []string {
	var stopped []string
	for _, name := range aiToolProcesses {
		cmd := exec.Command("taskkill", "/F", "/IM", name)
		out, _ := cmd.CombinedOutput()
		if strings.Contains(strings.ToLower(string(out)), "not found") {
			continue
		}
		stopped = append(stopped, name)
	}
	return stopped
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
