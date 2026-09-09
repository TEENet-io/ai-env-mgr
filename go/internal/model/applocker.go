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

// AppLockerMaxAllowPaths bounds how many allow rules one policy may carry.
// The console form is the normal way in, but policy.json is just a file in
// the store: a hand-edit -- or a compromised publisher -- must not be able
// to push hundreds of rules into every machine's security policy. 64 is far
// more than the handful of tool directories this list exists for. Both
// gates (console and agent) enforce it.
const AppLockerMaxAllowPaths = 64

var appLockerDriveRoot = regexp.MustCompile(`^[A-Za-z]:\\`)

// canonicalWholeDrive matches a canonicalised whole-drive rule (`c:\*`).
var canonicalWholeDrive = regexp.MustCompile(`^[a-z]:\\\*$`)

// appLockerAllowedVars are the AppLocker path variables that expand to
// machine-level, administrator-writable locations.
var appLockerAllowedVars = []string{`%PROGRAMFILES%\`, `%PROGRAMDATA%\`, `%OSDRIVE%\`}

// appLockerVarExpansions canonicalise the AppLocker path variables to the
// drive-letter spelling used by the safety test below, so that `C:\Windows\*`
// and `%OSDRIVE%\Windows\*` (and `%WINDIR%\*`) are all judged as the same
// location. The variable spellings stay acceptable *input*; expansion exists
// only so one location cannot be smuggled past the test under another name.
//
// %OSDRIVE% is assumed to be C: -- it is on every machine in this fleet, and
// assuming it makes the test stricter, never laxer.
var appLockerVarExpansions = [][2]string{
	{`%windir%\`, `c:\windows\`},
	{`%system32%\`, `c:\windows\system32\`},
	{`%programfiles%\`, `c:\program files\`},
	{`%programdata%\`, `c:\programdata\`},
	{`%osdrive%\`, `c:\`},
}

// forbiddenLocation is a canonicalised directory no allow rule may cover,
// with the reason, so the rationale lives next to the data.
type forbiddenLocation struct {
	prefix string
	why    string
}

// appLockerForbiddenLocations are locations no allow rule may name and no
// allow rule may contain. Most are user-writable: allowing execution from
// any of them would let an employee bypass the whole AppLocker policy by
// dropping a binary there.
//
// The whole Windows tree is on the list for a different reason. The image
// already allows `%WINDIR%\*` *with* an <Exceptions> block that removes
// wscript.exe, cscript.exe, cmd.exe, powershell.exe and mshta.exe. AppLocker
// unions allow rules and an <Exceptions> block constrains only the rule it
// hangs off, so any rule we add over the Windows tree would carry no
// exceptions and would hand every standard user the script hosts back --
// exactly the bypass the image's exception list exists to close. Nothing
// legitimate needs allow-listing under Windows.
var appLockerForbiddenLocations = []forbiddenLocation{
	{`c:\users\`, "user profiles are writable by the employee"},
	{`c:\windows\`, "the image already allows %WINDIR%\\* with the script-host exception list; a rule here would carry no exceptions and would re-enable wscript.exe, cmd.exe and powershell.exe for standard users"},
	{`%userprofile%`, "writable by the employee"},
	{`%localappdata%`, "writable by the employee"},
	{`%appdata%`, "writable by the employee"},
	{`%temp%`, "writable by the employee"},
	{`%tmp%`, "writable by the employee"},
	{`%public%`, "writable by every user"},
}

// canonicalAppLockerPath lower-cases p, normalises `/` to `\` and expands the
// AppLocker path variables to their drive-letter spelling, so that the safety
// test below sees one spelling per location. The result is only ever used for
// that test; the caller's original spelling is what gets written into the XML.
func canonicalAppLockerPath(p string) string {
	s := strings.ReplaceAll(strings.ToLower(p), "/", `\`)
	for _, e := range appLockerVarExpansions {
		if strings.HasPrefix(s, e[0]) {
			return e[1] + s[len(e[0]):]
		}
	}
	return s
}

// ValidateAppLockerPath reports why p is not an acceptable allow rule.
//
// The rule must name one machine-level directory tree: an absolute drive
// path or one of the allowed AppLocker variables, ending in `\*`, with no
// other wildcard, no `..` and no control characters.
//
// The safety test is deliberately wider than "refuse paths under a
// user-writable root" (which is how the original plan phrased it, and which
// is wrong): the rule is **refuse any path whose tree contains a forbidden
// location**, tested in both directions after canonicalisation.
//
//   - forbidden is a prefix of the candidate: `C:\Users\alice\*` is inside a
//     user profile.
//   - the candidate contains a forbidden location: `C:\*` contains
//     `C:\Users\`, `C:\Windows\*` contains `C:\Windows\Temp\`. Rejecting only
//     the first direction accepted the *parent* of every location it refused,
//     which voids the whole policy -- AppLocker unions allow rules, so one
//     such entry re-opens everything the image's exception list closed.
//
// Do not re-narrow this to one direction.
func ValidateAppLockerPath(p string) error {
	if p == "" {
		return fmt.Errorf("path is empty")
	}
	if len(p) > appLockerMaxPathLen {
		return fmt.Errorf("path is longer than %d characters", appLockerMaxPathLen)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path must not contain control characters")
		}
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

	canon := canonicalAppLockerPath(p)
	// Whole-drive and whole-ProgramData rules: parts of both are
	// user-writable, whichever spelling they arrive in.
	if canonicalWholeDrive.MatchString(canon) {
		return fmt.Errorf("a whole drive cannot be allowed")
	}
	if canon == `c:\programdata\*` {
		return fmt.Errorf("the whole ProgramData tree cannot be allowed; name one vendor directory under it")
	}
	// tree is the candidate's directory, ending in `\`: everything the rule
	// would allow lives under it.
	tree := strings.TrimSuffix(canon, `*`)
	for _, f := range appLockerForbiddenLocations {
		if strings.HasPrefix(canon, f.prefix) {
			return fmt.Errorf("path %q is under %s (%s)", p, strings.TrimSuffix(f.prefix, `\`), f.why)
		}
		if strings.HasPrefix(f.prefix, tree) {
			return fmt.Errorf("path %q covers %s (%s)", p, strings.TrimSuffix(f.prefix, `\`), f.why)
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
