package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// appLockerMaxPathLen bounds a rule path; AppLocker itself accepts longer
// strings, but nothing legitimate on these machines is near it.
const appLockerMaxPathLen = 200

var appLockerDriveRoot = regexp.MustCompile(`^[A-Za-z]:\\`)

// appLockerAllowedVars are the AppLocker path variables that expand to
// machine-level, administrator-writable locations.
var appLockerAllowedVars = []string{`%PROGRAMFILES%\`, `%PROGRAMDATA%\`, `%OSDRIVE%\`}

// appLockerForbiddenPrefixes are locations a standard user can write to.
// Allowing execution from any of them would let an employee bypass the
// whole AppLocker policy by dropping a binary there.
var appLockerForbiddenPrefixes = []string{
	`c:\users\`, `c:\windows\temp\`, `%userprofile%`, `%localappdata%`,
	`%appdata%`, `%temp%`, `%tmp%`, `%public%`,
}

// ValidateAppLockerPath reports why p is not an acceptable allow rule.
//
// The rule must name one machine-level directory tree: an absolute drive
// path or one of the allowed AppLocker variables, ending in `\*`, with no
// other wildcard and no `..`. Whole-drive and whole-ProgramData rules are
// refused because parts of both are user-writable.
func ValidateAppLockerPath(p string) error {
	if p == "" {
		return fmt.Errorf("path is empty")
	}
	if len(p) > appLockerMaxPathLen {
		return fmt.Errorf("path is longer than %d characters", appLockerMaxPathLen)
	}
	if !strings.HasSuffix(p, `\*`) {
		return fmt.Errorf(`path must end with \* (a directory tree)`)
	}
	body := strings.TrimSuffix(p, `*`)
	if strings.Contains(body, "*") || strings.Contains(p, "?") {
		return fmt.Errorf("only the trailing * is allowed")
	}
	if strings.Contains(p, `..`) {
		return fmt.Errorf("path must not contain ..")
	}
	lower := strings.ToLower(p)
	okRoot := appLockerDriveRoot.MatchString(p)
	for _, v := range appLockerAllowedVars {
		if strings.HasPrefix(lower, strings.ToLower(v)) {
			okRoot = true
		}
	}
	if !okRoot {
		return fmt.Errorf(`path must start with a drive letter (C:\...) or %%PROGRAMFILES%%, %%PROGRAMDATA%%, %%OSDRIVE%%`)
	}
	// Whole-drive / whole-variable rules: exactly "<root>\*".
	if appLockerDriveRoot.MatchString(p) && len(p) == len(`C:\*`) {
		return fmt.Errorf("a whole drive cannot be allowed")
	}
	if lower == `%programdata%\*` || lower == `%osdrive%\*` {
		return fmt.Errorf("a whole variable root cannot be allowed")
	}
	expanded := strings.Replace(lower, `%osdrive%\`, `c:\`, 1)
	for _, f := range appLockerForbiddenPrefixes {
		if strings.HasPrefix(expanded, f) {
			return fmt.Errorf("path %q is under a user-writable location (%s)", p, strings.TrimSuffix(f, `\`))
		}
	}
	return nil
}

// NormalizeAppLockerPaths trims, drops empties, removes case-insensitive
// duplicates (first spelling wins) and sorts case-insensitively, so the
// same list always serialises the same way and the agent's rewrite is
// idempotent.
func NormalizeAppLockerPaths(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k := strings.ToLower(p)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}
