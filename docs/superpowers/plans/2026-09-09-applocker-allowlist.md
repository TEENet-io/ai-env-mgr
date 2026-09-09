# AppLocker Allow-List (remote-managed) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the administrator publish a list of program directories that AppLocker must allow for every user, so a program installed outside `Program Files` (today: Codex at `C:\tools\Codex`) can be unblocked fleet-wide from the console without re-imaging.

**Architecture:** `policy.json` gains `appLockerAllowPaths`. On Windows the agent, inside the existing `policy.Apply` call, reads the machine's *local* AppLocker policy XML, replaces only the rules it owns (Id prefix `c0000000-0000-0000-0000-`, Name prefix `AIEnvMgr-allow-`) in the `Exe` collection with one Everyone/Allow `FilePathRule` per path, and writes the XML back with `Set-AppLockerPolicy -XmlPolicy` — the same call the image script uses. The console's 封禁策略 page gets an add/remove form with strict path validation. Machines without an AppLocker policy are left untouched (the agent never *enables* AppLocker).

**Tech Stack:** Go 1.25 (`encoding/xml` is NOT used — the policy XML is edited as text with a small, tested rewriter so the image's rules are preserved byte-for-byte), PowerShell `AppLocker` module via `exec.Command` (existing precedent in `internal/status/applocker_windows.go`), existing console patterns (`requirePost`, `publishPolicy`).

**Spec:** the design summary in this file's header plus the incident analysis recorded in `gateway-litellm/README.md` is the authority; there is no separate spec document. Root cause being fixed: image AppLocker rules allow Everyone only `%WINDIR%\*` and `%PROGRAMFILES%\*`; Codex is installed at `C:\tools\Codex`; in Enforce mode employees get "系统管理员已阻止这个应用".

## Global Constraints

- Repo: `/root/pp_home/windows-pc/ai-env-mgr/go`; work in the worktree `/root/pp_home/windows-pc/ai-env-mgr-applocker` (branch `applocker-allowlist`).
- Safety lines the agent must enforce in code, not by administrator care: never remove or alter any rule it does not own; never change `EnforcementMode`; never create an AppLocker policy where none exists; refuse paths under user-writable roots (see validation rules); on any parse/validation failure leave the machine's policy untouched and return an error.
- Validation rules for an allow path (shared by console and agent, `model.ValidateAppLockerPath`): must match `^[A-Za-z]:\\` or start with `%PROGRAMFILES%\`, `%PROGRAMDATA%\`, `%OSDRIVE%\`; must end with `\*`; the only `*` is the trailing one; no `..`; length ≤ 200; case-insensitive reject when the path (after variable expansion of `%OSDRIVE%`→`C:`) equals or is under any of: `C:\*`, `C:\Users\*`, `C:\Windows\Temp\*`, `%USERPROFILE%`, `%LOCALAPPDATA%`, `%APPDATA%`, `%TEMP%`, `%TMP%`, `%PUBLIC%`; also reject `%PROGRAMDATA%\*` (whole ProgramData is user-writable in parts) but allow subfolders of it.
- Managed rule shape (exact): `<FilePathRule Id="c0000000-0000-0000-0000-{index:012d}" Name="AIEnvMgr-allow-{index}" Description="managed by ai-env-mgr policy.json appLockerAllowPaths" UserOrGroupSid="S-1-1-0" Action="Allow"><Conditions><FilePathCondition Path="{path}" /></Conditions></FilePathRule>` inserted as the last children of `<RuleCollection Type="Exe" …>`.
- Commit messages: imperative, package-prefixed (`model:`, `policy:`, `agent:`, `console:`, `docs:`), ending with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`; no Claude-Session line.
- English code comments; Chinese UI copy. The `Applier` interface DOES gain one method (`ApplyAppLocker`) — see Task 3 for why the allow list cannot sit behind `ApplyPolicy`'s ETag gate.
- `go vet ./... && go test ./...` must pass before each commit; Windows-only files carry `//go:build windows` and must compile with `GOOS=windows go build ./...`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/model/model.go` (modify) | `Policy.AppLockerAllowPaths []string`; `Status.AppLockerAllowPaths int`. |
| `internal/model/applocker.go` (create) | `ValidateAppLockerPath`, `NormalizeAppLockerPaths` (dedupe, sort, case-insensitive). |
| `internal/model/applocker_test.go` (create) | Validation table tests. |
| `internal/policy/applocker.go` (create) | Pure XML rewriter `RewriteAppLockerXML(xml string, paths []string) (out string, changed bool, err error)`; constants for Id/Name prefixes; `AppLockerDeployed(xml) bool`. |
| `internal/policy/applocker_test.go` (create) | Rewriter tests with a fixture copied from the image script's XML. |
| `internal/policy/applocker_windows.go` (create) | `applyAppLocker(paths)`: `Get-AppLockerPolicy -Local -Xml` → rewrite → `Set-AppLockerPolicy -XmlPolicy` (temp file, UTF-16LE with BOM like the image script). |
| `internal/policy/registry_windows.go` (modify) | `Apply` calls `applyAppLocker` after the browser keys. |
| `internal/policy/registry_stub.go` (modify) | Non-Windows `Apply` unchanged (still errors); expose nothing new. |
| `cmd/agent/machine.go` (modify) | Status carries `AppLockerAllowPaths: len(pol.AppLockerAllowPaths)` — the count the agent *intends*; `agent status` prints it. |
| `internal/admincore/manager.go` (modify) | `MutateAppLockerAllowPaths(add, remove []string) (model.Policy, error)` with validation. |
| `internal/admincore/manager_test.go` (modify) | Tests. |
| `internal/adminweb/actions.go`, `server.go`, `assets/sites.html`, `actions_test.go` (modify) | `POST /sites/applocker`, form + list, CSRF test entry. |
| `docs/Codex分发方案.md`, `dist/部署说明.md`, `README.md` (modify) | Document the feature and the `C:\tools\Codex\*` remedy. |

---

### Task 1: Policy fields and path validation (`internal/model`)

**Files:**
- Modify: `internal/model/model.go` (`Policy`, `Status`)
- Create: `internal/model/applocker.go`, `internal/model/applocker_test.go`

**Interfaces:**
- Produces: `Policy.AppLockerAllowPaths []string \`json:"appLockerAllowPaths,omitempty"\``; `Status.AppLockerAllowPaths int \`json:"appLockerAllowPaths"\``
- Produces: `func ValidateAppLockerPath(p string) error`; `func NormalizeAppLockerPaths(paths []string) []string` (trim, drop empties, dedupe case-insensitively keeping first spelling, sort case-insensitively).

- [ ] **Step 1: Write the failing tests**

```go
package model

import "testing"

func TestValidateAppLockerPathAcceptsMachineLevelDirs(t *testing.T) {
	for _, p := range []string{
		`C:\tools\Codex\*`, `D:\Apps\Foo\*`, `%PROGRAMFILES%\Vendor\*`,
		`%PROGRAMDATA%\Vendor\App\*`, `%OSDRIVE%\tools\Codex\*`,
	} {
		if err := ValidateAppLockerPath(p); err != nil {
			t.Errorf("%q should be accepted: %v", p, err)
		}
	}
}

func TestValidateAppLockerPathRejectsUserWritableAndMalformed(t *testing.T) {
	for _, p := range []string{
		``, `C:\*`, `c:\users\alice\*`, `C:\Users\*`, `%USERPROFILE%\x\*`, `%LOCALAPPDATA%\Codex\*`,
		`%APPDATA%\x\*`, `%TEMP%\*`, `%TMP%\x\*`, `%PUBLIC%\x\*`, `C:\Windows\Temp\*`,
		`%PROGRAMDATA%\*`, `C:\tools\Codex`, `C:\tools\*\bin\*`, `C:\tools\..\Windows\*`,
		`\\server\share\*`, `tools\Codex\*`, `%OSDRIVE%\Users\bob\*`,
	} {
		if err := ValidateAppLockerPath(p); err == nil {
			t.Errorf("%q should be rejected", p)
		}
	}
	long := `C:\` + string(make([]byte, 200)) + `\*`
	if err := ValidateAppLockerPath(long); err == nil {
		t.Error("over-long path should be rejected")
	}
}

func TestNormalizeAppLockerPaths(t *testing.T) {
	got := NormalizeAppLockerPaths([]string{` C:\tools\Codex\* `, `c:\TOOLS\codex\*`, ``, `C:\Apps\Foo\*`})
	if len(got) != 2 || got[0] != `C:\Apps\Foo\*` || got[1] != `C:\tools\Codex\*` {
		t.Errorf("got %q", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/model/ -run AppLocker -v` → undefined functions.

- [ ] **Step 3: Implement**

Add to `Policy` (after `BlockedDomains`):

```go
	// AppLockerAllowPaths are program directories every user may execute
	// from, in AppLocker path syntax (`C:\tools\Codex\*`). The image ships
	// AppLocker allowing only Windows and Program Files; anything installed
	// elsewhere -- Codex at C:\tools\Codex -- is blocked for employees the
	// moment AppLocker is enforced. The agent turns this list into rules it
	// owns and never touches the image's own rules. Empty means "manage no
	// rules" (and remove any it previously wrote).
	AppLockerAllowPaths []string `json:"appLockerAllowPaths,omitempty"`
```

Add to `Status` after `AppLockerMode`: `AppLockerAllowPaths int \`json:"appLockerAllowPaths"\``.

Create `internal/model/applocker.go`:

```go
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
```

- [ ] **Step 4: Run tests** — `go test ./internal/model/ -v` → PASS. Also `go vet ./...`.

- [ ] **Step 5: Commit** — `model: policy gains AppLocker allow paths with strict validation`

---

### Task 2: Pure AppLocker XML rewriter (`internal/policy/applocker.go`)

**Files:**
- Create: `internal/policy/applocker.go`, `internal/policy/applocker_test.go`, `internal/policy/testdata/applocker_image.xml`

**Interfaces:**
- Produces:
  ```go
  const ManagedRuleIDPrefix = "c0000000-0000-0000-0000-"
  const ManagedRuleNamePrefix = "AIEnvMgr-allow-"
  func AppLockerDeployed(xml string) bool                       // has <AppLockerPolicy and a RuleCollection Type="Exe"
  func RewriteAppLockerXML(xml string, paths []string) (string, bool, error)
  ```
- Behaviour: removes every `<FilePathRule …>…</FilePathRule>` whose `Id` starts with `ManagedRuleIDPrefix` OR whose `Name` starts with `ManagedRuleNamePrefix` anywhere in the document; then, if `paths` is non-empty, inserts the managed rules (exact shape from Global Constraints, index from 1) immediately before the `</RuleCollection>` that closes the `Exe` collection. Returns `changed=false` when the output equals the input. Errors: no `<AppLockerPolicy`, no `Exe` collection, or an unbalanced managed rule (start without end). It must not modify any other byte (whitespace of untouched rules preserved), and must not touch `EnforcementMode`.

- [ ] **Step 1: Fixture** — copy the `<AppLockerPolicy …>…</AppLockerPolicy>` block from `scripts/02-Manage-AIAccess.ps1` (lines 108–160, with `$Mode` replaced by `Enabled`, `$AdminsSid` by `S-1-5-32-544`, `$EveryoneSid` by `S-1-1-0`) into `internal/policy/testdata/applocker_image.xml`.

- [ ] **Step 2: Write the failing tests**

```go
package policy

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/applocker_image.xml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRewriteAddsManagedRulesToExeCollectionOnly(t *testing.T) {
	out, changed, err := RewriteAppLockerXML(fixture(t), []string{`C:\tools\Codex\*`, `D:\Apps\Foo\*`})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	exe := out[strings.Index(out, `<RuleCollection Type="Exe"`):strings.Index(out, `<RuleCollection Type="Script"`)]
	if strings.Count(exe, ManagedRuleNamePrefix) != 2 {
		t.Errorf("expected 2 managed rules in Exe collection:\n%s", exe)
	}
	if !strings.Contains(exe, `Id="c0000000-0000-0000-0000-000000000001" Name="AIEnvMgr-allow-1"`) ||
		!strings.Contains(exe, `<FilePathCondition Path="C:\tools\Codex\*" />`) ||
		!strings.Contains(exe, `UserOrGroupSid="S-1-1-0" Action="Allow"`) {
		t.Errorf("managed rule shape wrong:\n%s", exe)
	}
	script := out[strings.Index(out, `<RuleCollection Type="Script"`):]
	if strings.Contains(script, ManagedRuleNamePrefix) {
		t.Error("managed rules must not leak into other collections")
	}
	// Everything the image wrote is still there, byte for byte.
	for _, keep := range []string{`Admins-allow-all`, `Everyone-allow-Windows`, `Everyone-allow-ProgramFiles`, `EnforcementMode="Enabled"`, `%SYSTEM32%\reg.exe`} {
		if strings.Count(out, keep) != strings.Count(fixture(t), keep) {
			t.Errorf("image rule %q was altered", keep)
		}
	}
}

func TestRewriteIsIdempotentAndReplacesOldManagedRules(t *testing.T) {
	once, _, _ := RewriteAppLockerXML(fixture(t), []string{`C:\tools\Codex\*`})
	twice, changed, err := RewriteAppLockerXML(once, []string{`C:\tools\Codex\*`})
	if err != nil || changed || twice != once {
		t.Fatalf("second pass must be a no-op: changed=%v err=%v", changed, err)
	}
	replaced, changed, _ := RewriteAppLockerXML(once, []string{`D:\Apps\Foo\*`})
	if !changed || strings.Contains(replaced, `C:\tools\Codex`) || strings.Count(replaced, ManagedRuleNamePrefix) != 1 {
		t.Errorf("old managed rule must be replaced, not accumulated:\n%s", replaced)
	}
}

func TestRewriteWithNoPathsRemovesManagedRules(t *testing.T) {
	once, _, _ := RewriteAppLockerXML(fixture(t), []string{`C:\tools\Codex\*`})
	back, changed, err := RewriteAppLockerXML(once, nil)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if back != fixture(t) {
		t.Error("removing all managed rules must restore the image XML exactly")
	}
	same, changed, _ := RewriteAppLockerXML(fixture(t), nil)
	if changed || same != fixture(t) {
		t.Error("no managed rules and no paths must be a no-op")
	}
}

func TestRewriteRefusesForeignOrMissingPolicy(t *testing.T) {
	if _, _, err := RewriteAppLockerXML(``, []string{`C:\x\*`}); err == nil {
		t.Error("empty document must be refused")
	}
	if _, _, err := RewriteAppLockerXML(`<AppLockerPolicy Version="1"></AppLockerPolicy>`, []string{`C:\x\*`}); err == nil {
		t.Error("policy without an Exe collection must be refused, not created")
	}
	if AppLockerDeployed(`<AppLockerPolicy Version="1"></AppLockerPolicy>`) || !AppLockerDeployed(fixture(t)) {
		t.Error("AppLockerDeployed wrong")
	}
}

func TestRewriteEscapesXMLInPaths(t *testing.T) {
	out, _, err := RewriteAppLockerXML(fixture(t), []string{`C:\tools\A&B\*`})
	if err != nil || !strings.Contains(out, `Path="C:\tools\A&amp;B\*"`) {
		t.Errorf("ampersand must be escaped: %v\n%s", err, out)
	}
}
```

- [ ] **Step 3: Run to verify failure** — `go test ./internal/policy/ -run Rewrite -v` → undefined.

- [ ] **Step 4: Implement `internal/policy/applocker.go`**

```go
package policy

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// Managed AppLocker rules are the only ones the agent writes or removes.
// Both prefixes are checked so a rule renamed by hand is still recognised
// as ours, and so nothing the image wrote (Ids a…/b…/d…-prefixed) is ever
// touched.
const (
	ManagedRuleIDPrefix   = "c0000000-0000-0000-0000-"
	ManagedRuleNamePrefix = "AIEnvMgr-allow-"
)

// managedRule matches one complete FilePathRule element the agent owns.
// Rules are simple enough (no nesting of the same element) for a regexp.
var managedRule = regexp.MustCompile(`(?s)[ \t]*<FilePathRule\b[^>]*\b(?:Id="` +
	regexp.QuoteMeta(ManagedRuleIDPrefix) + `[^"]*"|Name="` +
	regexp.QuoteMeta(ManagedRuleNamePrefix) + `[^"]*")[^>]*>.*?</FilePathRule>\r?\n?`)

var exeCollectionOpen = regexp.MustCompile(`<RuleCollection\s+Type="Exe"[^>]*>`)

// AppLockerDeployed reports whether xml is a policy the agent may add rules
// to: an AppLocker document that already has an Exe collection. A machine
// without one has AppLocker undeployed or cleared, and the agent must not
// be the thing that switches it on.
func AppLockerDeployed(xml string) bool {
	return strings.Contains(xml, "<AppLockerPolicy") && exeCollectionOpen.MatchString(xml)
}

// RewriteAppLockerXML returns xml with the agent's managed rules replaced by
// one Everyone/Allow path rule per entry in paths, appended at the end of
// the Exe collection. Nothing else in the document is changed, including
// EnforcementMode. changed is false when the result equals the input.
func RewriteAppLockerXML(xml string, paths []string) (string, bool, error) {
	if !AppLockerDeployed(xml) {
		return "", false, fmt.Errorf("no AppLocker policy with an Exe rule collection; refusing to create one")
	}
	if strings.Count(xml, "<FilePathRule") != strings.Count(xml, "</FilePathRule>") {
		return "", false, fmt.Errorf("unbalanced FilePathRule elements; refusing to edit")
	}
	stripped := managedRule.ReplaceAllString(xml, "")

	if len(paths) > 0 {
		open := exeCollectionOpen.FindStringIndex(stripped)
		rel := strings.Index(stripped[open[1]:], "</RuleCollection>")
		if rel < 0 {
			return "", false, fmt.Errorf("Exe rule collection is not closed")
		}
		closeAt := open[1] + rel
		var b strings.Builder
		for i, p := range paths {
			fmt.Fprintf(&b, "    <FilePathRule Id=\"%s%012d\" Name=\"%s%d\" Description=\"managed by ai-env-mgr policy.json appLockerAllowPaths\" UserOrGroupSid=\"S-1-1-0\" Action=\"Allow\">\n      <Conditions><FilePathCondition Path=\"%s\" /></Conditions>\n    </FilePathRule>\n",
				ManagedRuleIDPrefix, i+1, ManagedRuleNamePrefix, i+1, html.EscapeString(p))
		}
		// Insert before the indentation of the closing tag so the result
		// keeps the image's layout.
		lineStart := strings.LastIndex(stripped[:closeAt], "\n") + 1
		stripped = stripped[:lineStart] + b.String() + stripped[lineStart:]
	}
	return stripped, stripped != xml, nil
}
```

Note: `html.EscapeString` escapes `& < > " '` which is what XML attribute values need here.

- [ ] **Step 5: Run tests** — `go test ./internal/policy/ -v` → PASS. If `TestRewriteWithNoPathsRemovesManagedRules` fails on whitespace, adjust the trailing `\r?\n?` and leading indentation capture in `managedRule` until removal restores the fixture exactly — the test is the contract.

- [ ] **Step 6: Commit** — `policy: rewrite AppLocker XML to carry managed allow rules`

---

### Task 3: Apply the allow list every sync cycle (Windows + agentcore)

**Files:**
- Create: `internal/policy/applocker_windows.go`, `internal/policy/applocker_stub.go`
- Modify: `internal/agentcore/sync.go` (`Applier` interface + call site), `internal/agentcore/sync_test.go` (fake + tests), `cmd/agent/machine.go` (`localApplier`, status), `cmd/agent/main.go` (status print)

**Why this is not inside `policy.Apply`:** the browser keys are behind an ETag gate in
`sync.go` — when the policy object has not changed, `ApplyPolicy` is skipped entirely.
The allow list must NOT be behind that gate. `ApplyAppLocker` already reads the machine's
own XML and no-ops when it matches, so the gate would buy nothing while stopping the
machine from ever self-healing: if the image script is re-run, a GPO refresh replaces the
local policy, or an administrator edits rules by hand, the allow rule would stay gone until
somebody happened to edit the policy object. So it becomes its own `Applier` method,
called on every cycle.

**Interfaces:**
- Produces: `func ApplyAppLocker(paths []string) error` in `internal/policy` (exported; Windows real, non-Windows no-op).
- Produces: `var ErrAppLockerNotDeployed = errors.New(...)` in `internal/policy` — returned when the machine has no AppLocker policy to add rules to, so the caller can report it as a warning instead of an error.
- Produces: `Applier` interface gains `ApplyAppLocker(paths []string) error`.
- Produces: `Status.AppLockerAllowPaths int` is filled from the policy the agent just read.

- [ ] **Step 1: Write the failing agentcore tests**

In `internal/agentcore/sync_test.go`, add to `fakeApplier`:

```go
	appLockerPaths [][]string
	appLockerErr   error
```

and the method:

```go
func (a *fakeApplier) ApplyAppLocker(paths []string) error {
	a.appLockerPaths = append(a.appLockerPaths, paths)
	return a.appLockerErr
}
```

Then the tests (use whatever helper the file already has for building a Syncer and seeding
a policy object — follow the existing tests in this file, do not invent a new harness):

```go
func TestAppLockerAllowListIsAppliedEvenWhenThePolicyIsUnchanged(t *testing.T) {
	// The browser keys are skipped on an unchanged ETag. The allow list must
	// not be: it is the only thing that lets an employee launch Codex, and a
	// machine whose local policy was replaced has to converge on its own.
	// (Seed a policy with AppLockerAllowPaths, run two cycles, assert
	// ApplyPolicy ran once and ApplyAppLocker ran twice with the same paths.)
}

func TestAppLockerNotDeployedIsAWarningNotAnError(t *testing.T) {
	// A machine that never had AppLocker is a normal state, not a failure.
	// (fake returns policy.ErrAppLockerNotDeployed; assert the status carries
	// it in Warns and NOT in Errors.)
}

func TestAppLockerApplyFailureIsReportedAsAnError(t *testing.T) {
	// Any other failure means the machine is not in the state we published.
	// (fake returns errors.New("boom"); assert it lands in Errors.)
}
```

Write the bodies to match this file's existing style. If the file's Warns plumbing differs
from what these names suggest, follow the file and say so in your report.

- [ ] **Step 2: Run to verify failure** — `go test ./internal/agentcore/ -run AppLocker -v` → fake does not satisfy `Applier` / undefined.

- [ ] **Step 3: Implement `internal/policy/applocker_windows.go`**

```go
//go:build windows

package policy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// ErrAppLockerNotDeployed means this machine has no AppLocker policy with an
// Exe rule collection. The agent never creates one -- whether AppLocker is
// deployed at all is the image's decision -- so callers should report this
// as a warning, not a failure.
var ErrAppLockerNotDeployed = errors.New("AppLocker is not deployed on this machine")

// ApplyAppLocker brings the machine's LOCAL AppLocker policy in line with
// paths (see RewriteAppLockerXML).
//
// It reads the LOCAL policy rather than the effective one: the effective
// policy folds in domain GPOs, and writing that back would copy someone
// else's rules into our local store, where they would then outlive the GPO.
//
// It is called on every sync cycle, so it must stay cheap and quiet when
// nothing has drifted: one read, and a write only when the XML actually
// changes.
func ApplyAppLocker(paths []string) error {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", "Import-Module AppLocker; Get-AppLockerPolicy -Local -Xml").Output()
	if err != nil {
		if len(paths) == 0 {
			// Nothing to manage and nothing readable: not worth a status line.
			return nil
		}
		return fmt.Errorf("read local AppLocker policy: %w", err)
	}
	xml := strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff"))
	if !AppLockerDeployed(xml) {
		if len(paths) == 0 {
			return nil
		}
		return fmt.Errorf("%w; %d allow path(s) not applied", ErrAppLockerNotDeployed, len(paths))
	}
	next, changed, err := RewriteAppLockerXML(xml, paths)
	if err != nil {
		return fmt.Errorf("rewrite AppLocker policy: %w", err)
	}
	if !changed {
		return nil
	}
	tmp := filepath.Join(os.TempDir(), "aienvmgr-applocker.xml")
	if err := os.WriteFile(tmp, utf16LEWithBOM(next), 0o600); err != nil {
		return fmt.Errorf("write AppLocker policy temp file: %w", err)
	}
	defer os.Remove(tmp)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", fmt.Sprintf("Import-Module AppLocker; Set-AppLockerPolicy -XmlPolicy '%s' -ErrorAction Stop", tmp))
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Set-AppLockerPolicy: %v: %s", err, strings.TrimSpace(string(msg)))
	}
	return nil
}

// utf16LEWithBOM encodes the way the image script writes the policy file
// (Out-File -Encoding Unicode), which Set-AppLockerPolicy reads reliably.
func utf16LEWithBOM(s string) []byte {
	u := utf16.Encode([]rune(s))
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xFE})
	for _, r := range u {
		b.WriteByte(byte(r))
		b.WriteByte(byte(r >> 8))
	}
	return b.Bytes()
}
```

Note: in the real file the BOM strip must be the actual U+FEFF rune literal, not the
escaped text shown above.

- [ ] **Step 4: Implement `internal/policy/applocker_stub.go`**

```go
//go:build !windows

package policy

import "errors"

// ErrAppLockerNotDeployed mirrors the Windows sentinel so callers compile
// and switch on it everywhere.
var ErrAppLockerNotDeployed = errors.New("AppLocker is not deployed on this machine")

// ApplyAppLocker is a no-op off Windows.
//
// Unlike Apply, which returns an error because calling it off Windows is a
// programming mistake, this one is invoked unconditionally on every sync
// cycle; erroring would turn every cycle of a non-Windows build into a
// status full of noise for a machine that has no AppLocker to manage.
func ApplyAppLocker(paths []string) error { return nil }
```

- [ ] **Step 5: Wire it into the cycle**

`internal/agentcore/sync.go`: add to the `Applier` interface, with a comment saying why it
is separate from `ApplyPolicy` (self-healing, not ETag-gated). At the call site, after the
policy is parsed successfully — inside the `else` that currently splits into
"unchanged"/"apply" — restructure so the AppLocker call happens in BOTH paths, e.g. parse
once, then:

```go
		} else {
			if etag != "" && etag == s.readMarker(policyMarkerFile) {
				// Unchanged since the last cycle; skip the registry writes.
			} else if err := s.Applier.ApplyPolicy(pol); err != nil {
				errs = append(errs, fmt.Sprintf("policy apply: %v", err))
			} else {
				s.writeMarker(policyMarkerFile, etag)
			}
			// Not gated by the ETag: see the Applier comment.
			if err := s.Applier.ApplyAppLocker(pol.AppLockerAllowPaths); err != nil {
				if errors.Is(err, policy.ErrAppLockerNotDeployed) {
					warns = append(warns, fmt.Sprintf("applocker: %v", err))
				} else {
					errs = append(errs, fmt.Sprintf("applocker: %v", err))
				}
			}
		}
```

Use the file's actual warning slice name and its existing import of `internal/policy` (add
the import if absent — check first that this does not create an import cycle; `agentcore`
importing `policy` is fine, `policy` must not import `agentcore`).

`cmd/agent/machine.go`: `func (localApplier) ApplyAppLocker(paths []string) error { return policy.ApplyAppLocker(paths) }`, and fill `AppLockerAllowPaths: len(pol.AppLockerAllowPaths)` where the status is built (follow how `AppLockerMode` gets there).

`cmd/agent/main.go`: append ` applocker_allow=%d` to the two status prints that already show `applocker=%s`.

- [ ] **Step 6: Run** — `go vet ./... && go test ./...` → PASS; `GOOS=windows GOARCH=amd64 go build ./...` → PASS.

- [ ] **Step 7: Commit** — `agent: apply the AppLocker allow list on every sync cycle`

---

### Task 4: Console form and mutator

**Files:**
- Modify: `internal/admincore/manager.go`, `internal/admincore/manager_test.go`, `internal/adminweb/actions.go`, `internal/adminweb/server.go`, `internal/adminweb/assets/sites.html`, `internal/adminweb/actions_test.go`

**Interfaces:**
- Produces: `func (m *Manager) MutateAppLockerAllowPaths(add, remove []string) (model.Policy, error)` — validates every `add` with `model.ValidateAppLockerPath` (first error aborts, nothing published), removes case-insensitively, normalises, publishes.
- Route: `POST /sites/applocker` (`requirePost("/sites", s.actionAppLocker)`), form fields `add`, `remove` (textarea, split like domains but on newlines/commas/semicolons only — paths contain spaces).

- [ ] **Step 1: Tests**

`manager_test.go`:
```go
func TestMutateAppLockerAllowPathsValidatesAndNormalises(t *testing.T) {
	m, _ := newManager()
	p, err := m.MutateAppLockerAllowPaths([]string{`c:\TOOLS\codex\*`, `C:\tools\Codex\*`, `D:\Apps\Foo\*`}, nil)
	if err != nil || len(p.AppLockerAllowPaths) != 2 {
		t.Fatalf("got %v %v", p.AppLockerAllowPaths, err)
	}
	if _, err := m.MutateAppLockerAllowPaths([]string{`%LOCALAPPDATA%\x\*`}, nil); err == nil {
		t.Fatal("user-writable path must be refused and nothing published")
	}
	p, _ = m.MutateAppLockerAllowPaths(nil, []string{`C:\TOOLS\CODEX\*`})
	if len(p.AppLockerAllowPaths) != 1 || p.AppLockerAllowPaths[0] != `D:\Apps\Foo\*` {
		t.Errorf("remove must be case-insensitive: %v", p.AppLockerAllowPaths)
	}
}
```
`actions_test.go`: add `{"/sites/applocker", url.Values{"add": {`C:\tools\Codex\*`}}}` to the CSRF list; and a `splitPaths` unit test: input "C:\\a b\\*\nC:\\c\\*, D:\\d\\*" → 3 items, spaces preserved.

- [ ] **Step 2: Implement**

`manager.go`:
```go
// MutateAppLockerAllowPaths edits the directories AppLocker must allow for
// every user. Invalid additions abort the whole change: a half-applied
// allow list is exactly the kind of drift the validation exists to stop.
func (m *Manager) MutateAppLockerAllowPaths(add, remove []string) (model.Policy, error) {
	p, err := m.CurrentPolicy()
	if err != nil {
		return model.Policy{}, err
	}
	for _, a := range add {
		if err := model.ValidateAppLockerPath(strings.TrimSpace(a)); err != nil {
			return model.Policy{}, fmt.Errorf("allow path %q: %w", a, err)
		}
	}
	drop := map[string]bool{}
	for _, r := range remove {
		drop[strings.ToLower(strings.TrimSpace(r))] = true
	}
	var keep []string
	for _, existing := range append(append([]string{}, p.AppLockerAllowPaths...), add...) {
		if !drop[strings.ToLower(strings.TrimSpace(existing))] {
			keep = append(keep, existing)
		}
	}
	p.AppLockerAllowPaths = model.NormalizeAppLockerPaths(keep)
	return m.publishPolicy(p)
}
```
`actions.go`:
```go
// splitPaths splits on line and list separators only; Windows paths may
// contain spaces, so unlike domains a space is not a separator.
func splitPaths(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' || r == ';' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (s *Server) actionAppLocker(sess *session, r *http.Request) error {
	add, remove := splitPaths(formValue(r, "add")), splitPaths(formValue(r, "remove"))
	if len(add) == 0 && len(remove) == 0 {
		return fmt.Errorf("nothing to add or remove")
	}
	_, err := sess.mgr.MutateAppLockerAllowPaths(add, remove)
	return err
}
```
`server.go`: `mux.HandleFunc("/sites/applocker", s.requirePost("/sites", s.actionAppLocker))`.

`sites.html`: after the 增删域名 form, inside a `{{with .Policy}}` block for the list:
```html
  <h2>AppLocker 放行目录</h2>
  {{with .Policy}}
  <div class="panel scroll">
  <table>
    <tbody>
    {{range .AppLockerAllowPaths}}<tr><td class="id">{{.}}</td></tr>
    {{else}}<tr><td class="empty">没有额外放行目录；员工只能运行 Windows 与 Program Files 下的程序。</td></tr>{{end}}
    </tbody>
  </table>
  </div>
  {{end}}
  <form method="post" action="/sites/applocker" class="form">
    <input type="hidden" name="csrf" value="{{.CSRF}}">
    <label class="wide">新增<textarea name="add" rows="2" placeholder="每行一个，如 C:\tools\Codex\*"></textarea></label>
    <label class="wide">移除<textarea name="remove" rows="2"></textarea></label>
    <button type="submit">提交</button>
  </form>
  <p class="hint">
    镜像里的 AppLocker 只允许员工运行 Windows 与 Program Files 下的程序；装在别处的程序（如 <code>C:\tools\Codex</code>）
    在强制模式下会提示「系统管理员已阻止这个应用」。这里的目录由 agent 在下次同步时写成 AppLocker 放行规则，
    对机器上所有用户生效。<strong>只接受机器级目录</strong>：用户目录、AppData、Temp、整盘一律拒绝，否则白名单形同虚设。
    目录必须以 <code>\*</code> 结尾。没有部署 AppLocker 的机器不受影响。
  </p>
```

- [ ] **Step 3: Run** — `go vet ./... && go test ./...` → PASS; `ADMINWEB_PREVIEW_DIR=… go test ./internal/adminweb/ -run TestRenderPreview` renders sites.html.

- [ ] **Step 4: Commit** — `console: manage the AppLocker allow list from the policy page`

---

### Task 5: Docs and release

**Files:**
- Modify: `docs/Codex分发方案.md`, `dist/部署说明.md`, `README.md`
- Create: `docs/AppLocker与Codex启动.md`

**What changed since this plan was written.** The incident was diagnosed on a real machine
on 2026-09-09 and the root cause is NOT what the plan's header assumed. Codex is launched
as `C:\Windows\system32\wscript.exe "C:\Tools\Codex\Codex.vbs"`. The image's Exe rule
allows `%WINDIR%\*` but **excepts `%SYSTEM32%\wscript.exe`** (alongside cmd, powershell,
cscript, mshta) precisely to stop script hosts being used to bypass the policy — see
`scripts/02-Manage-AIAccess.ps1:114-132`. So the launch is refused at `wscript.exe`, before
Codex is reached. `Codex.vbs` would then also fail the Script collection, which allows only
`%WINDIR%\*` and `%PROGRAMFILES%\*` scripts. The allow list this branch adds is still
necessary — the real binary is `C:\Tools\Codex\_internal\app\ChatGPT.exe`, outside the
image's whitelist — but it is not sufficient on its own. Document both halves; do not
soften the image's exception list.

`Codex.vbs` does only two things (read off the machine): it sets
`CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE=1` and starts `_internal\app\ChatGPT.exe` with
that directory as the working directory. Both are reproducible without a script host.

- [ ] **Step 1: Write `docs/AppLocker与Codex启动.md`**

A short Chinese document, structured as: 现象 (what the employee sees) → 根因 (the two
blocks in series, with the file:line reference into the image script) → 为什么不放行
wscript (`C:\Windows\Temp` is user-writable and the Script collection allows `%WINDIR%\*`,
so re-enabling wscript reopens exactly the bypass the exception list exists to prevent) →
正确做法 (machine-level environment variable + shortcut straight to `ChatGPT.exe`, plus the
allow path `C:\tools\Codex\*` published from the console) → 验证 (`agent status` shows
`applocker_allow=1`, and no new AppLocker 8004 event for the launch). Record that
`applocker_allow=?` means the agent could not read the local policy, which is a different
state from `0`.

End with a 遗留 section naming the two things this branch does NOT do, so they are not
forgotten:
1. The machine-level environment variable is set by hand today. The agent already writes
   HKLM policy keys, so managing a small set of machine environment variables from
   `policy.json` is the natural home for it. Not in this branch.
2. The codex-kiosk installer still creates a `wscript.exe` + `.vbs` shortcut. The durable
   fix is for it to create a shortcut to `ChatGPT.exe` directly. That is a change in the
   `TEENet-io/codex-kiosk` repository, not this one.

- [ ] **Step 2: `dist/部署说明.md`** — add to the symptom table:
`员工打开 Codex 提示"系统管理员已阻止这个应用" → 见 docs/AppLocker与Codex启动.md。两个原因要一起解决：启动走 wscript.exe（被镜像 AppLocker 故意排除），且 Codex 目录不在白名单内。`
Also document the new `policy.json` field `appLockerAllowPaths` alongside the existing
fields, and the `applocker_allow=N|?` line in `agent status`.

- [ ] **Step 3: `docs/Codex分发方案.md`** — after the `C:\tools\Codex` paragraph, add:
`该目录不在镜像 AppLocker 的白名单内。agent 1.2.13 起由 policy.json 的 appLockerAllowPaths 放行（控制台「封禁策略」页），装机脚本无需改。注意真正的可执行文件是 _internal\app\ChatGPT.exe，且默认快捷方式经 wscript.exe 中转——后者会被 AppLocker 拦下，见 docs/AppLocker与Codex启动.md。`

- [ ] **Step 4: `README.md`** — one line in the feature list: 控制台可下发 AppLocker 放行目录，agent 每个同步周期校对一次。

- [ ] **Step 5: Commit** — `docs: record why Codex is blocked and how the allow list fixes it`

- [ ] **Step 6: Merge and release** (controller performs; do not do this as the task implementer)
Merge `applocker-allowlist` into `main` (fast-forward), run the full suite on the merged
tree, tag `v1.2.13` and push (release.yml builds agent.exe), then build and deploy the
console as `web-32` following the web-31 procedure: `scp` the binary to
`/opt/ai-env-mgr/web-32` on 47.236.115.50, repoint the `/opt/ai-env-mgr/admin` symlink,
`systemctl restart ai-env-mgr-admin`.

- [ ] **Step 7: Remediate the fleet** (administrator)
Publish agent 1.2.13 from the console rollout page; on 封禁策略 add `C:\tools\Codex\*`;
apply the launcher fix (machine environment variable + shortcut to `ChatGPT.exe`); have the
employee sign out and back in. Verify with `agent status`: `applocker_allow=1`.

---

## Self-Review

- Spec coverage: fields+validation (T1), rewriter with preservation/idempotence/removal (T2), Windows apply + never-enable + status (T3), console UI + mutator + CSRF (T4), docs/release/remediation (T5). Safety lines: only managed rules touched (T2 tests), EnforcementMode untouched (T2 test), never create policy (T2/T3), user-writable paths refused (T1), failure leaves policy untouched (T3 returns before writing). ✔
- Placeholders: none.
- Type consistency: `Policy.AppLockerAllowPaths []string`, `Status.AppLockerAllowPaths int`, `ValidateAppLockerPath`, `NormalizeAppLockerPaths`, `RewriteAppLockerXML(xml, paths) (string, bool, error)`, `AppLockerDeployed`, `applyAppLocker(paths) error`, `MutateAppLockerAllowPaths(add, remove)`, `splitPaths`, `actionAppLocker`, route `/sites/applocker` — consistent across tasks. ✔
