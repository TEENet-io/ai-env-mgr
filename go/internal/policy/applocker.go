// Package policy's AppLocker support rewrites the machine's local AppLocker
// XML to add or remove rules the agent owns, without disturbing anything
// else in the document -- including the image's own rules and
// EnforcementMode. This file is pure string/XML manipulation; it does not
// touch the registry or any Windows API (see registry_windows.go for that).
package policy

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// Set-AppLockerPolicy takes the new policy as a file path, and the agent
// runs as LocalSystem: where that file is staged is a security decision, not
// a detail. os.TempDir() for LocalSystem is C:\Windows\Temp -- a directory
// this very branch's validator classifies as user-writable -- and the file
// was written under a fixed name. SYSTEM wrote it and a separate process
// (powershell.exe) read it back, so a standard user who pre-created or held
// that name, or put a link there, could swap the contents in between and get
// SYSTEM to install an AppLocker policy of their choosing. 0o600 buys
// nothing against that on Windows.
//
// The staging directory must therefore be writable by SYSTEM and
// Administrators only; the agent's own state directory (%ProgramData%\AIEnvMgr,
// created by the agent as SYSTEM) is exactly that. The policy package cannot
// import cmd/agent, so the agent supplies it here at startup; the default is
// derived the same way, and when even that cannot be determined ApplyAppLocker
// refuses rather than falling back somewhere world-writable.
var (
	stagingMu     sync.RWMutex
	stagingDirSet string
)

// SetAppLockerStagingDir tells ApplyAppLocker which directory to stage the
// policy XML in. Call it once at startup, before any sync cycle runs.
func SetAppLockerStagingDir(dir string) {
	stagingMu.Lock()
	defer stagingMu.Unlock()
	stagingDirSet = dir
}

// appLockerStagingDir returns the directory to stage the policy XML in: what
// SetAppLockerStagingDir was given, else %ProgramData%\AIEnvMgr, else an
// error. It never returns a temp directory: see the comment above.
func appLockerStagingDir() (string, error) {
	stagingMu.RLock()
	dir := stagingDirSet
	stagingMu.RUnlock()
	if dir != "" {
		return dir, nil
	}
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "AIEnvMgr"), nil
	}
	return "", fmt.Errorf("no directory to stage the AppLocker policy in: %%ProgramData%% is unset and no staging directory was set")
}

// Managed AppLocker rules are the only ones the agent writes or removes.
// Both prefixes are checked so a rule renamed by hand is still recognised
// as ours.
//
// The image's four rule collections use Id prefixes a/b/c/d, one per
// collection (Exe/Script/Msi/Appx -- see scripts/02-Manage-AIAccess.ps1).
// ManagedRuleIDPrefix uses "e" precisely because it is not one of those:
// the agent's Id can never collide with an image-written rule's Id, in the
// Exe collection or in any collection a future change might touch.
//
// That uniqueness is belt; matching (and therefore removal) is also scoped
// to the Exe rule collection only -- see RewriteAppLockerXML -- which is
// braces: the agent never creates or removes rules outside the Exe
// collection anyway, so even a prefix collision could not reach another
// collection's rules. Neither defence alone should be relied on to the
// exclusion of the other.
const (
	ManagedRuleIDPrefix   = "e0000000-0000-0000-0000-"
	ManagedRuleNamePrefix = "AIEnvMgr-allow-"
)

// managedRule matches one complete FilePathRule element the agent owns,
// including the indentation before it and the newline after it, so that
// removing every match restores the text exactly as it was before any
// managed rule was ever inserted. Rules are simple enough (no nesting of
// the same element) for a regexp.
var managedRule = regexp.MustCompile(`(?s)[ \t]*<FilePathRule\b[^>]*\b(?:Id="` +
	regexp.QuoteMeta(ManagedRuleIDPrefix) + `[^"]*"|Name="` +
	regexp.QuoteMeta(ManagedRuleNamePrefix) + `[^"]*")[^>]*>.*?</FilePathRule>\r?\n?`)

// exeCollectionTag matches an Exe rule-collection tag, self-closing or not.
var exeCollectionTag = regexp.MustCompile(`<RuleCollection\s+Type="Exe"[^>]*>`)

// exeCollectionOpens returns the locations of the Exe rule-collection tags
// that actually open a collection.
//
// Get-AppLockerPolicy -Local -Xml always returns a document containing all
// five rule collections; the ones that were never configured come back
// self-closing (`<RuleCollection Type="Exe" EnforcementMode="NotConfigured" />`).
// `[^>]*` happily eats the `/`, so a machine with no AppLocker at all used to
// look deployed: every cycle then failed with "Exe rule collection is not
// closed" (even with zero allow paths published), and a document whose Exe
// collection was empty but whose Msi collection was not had rules inserted
// *outside* any collection. A self-closing tag opens nothing.
func exeCollectionOpens(xml string) [][]int {
	var out [][]int
	for _, loc := range exeCollectionTag.FindAllStringIndex(xml, -1) {
		if strings.HasSuffix(xml[loc[0]:loc[1]], "/>") {
			continue
		}
		out = append(out, loc)
	}
	return out
}

// filePathConditionPath extracts the Path attribute of the (first)
// FilePathCondition inside a matched FilePathRule element.
var filePathConditionPath = regexp.MustCompile(`<FilePathCondition\s+Path="([^"]*)"`)

// AppLockerDeployed reports whether xml is a policy the agent may add rules
// to: an AppLocker document that already has an Exe collection. A machine
// without one has AppLocker undeployed or cleared, and the agent must not
// be the thing that switches it on.
func AppLockerDeployed(xml string) bool {
	return strings.Contains(xml, "<AppLockerPolicy") && len(exeCollectionOpens(xml)) > 0
}

// sliceExeCollection splits xml around the body of its Exe rule collection:
// before is everything up to and including the opening tag, body is the
// collection's contents, and after is everything from the closing tag
// onward. Both RewriteAppLockerXML and ManagedPaths use this so there is
// exactly one place that locates the Exe collection's boundaries.
//
// The error says why the document cannot be sliced: no open Exe collection,
// no closing tag, or another rule collection starting before that closing
// tag -- which means the `</RuleCollection>` found belongs to a later
// collection and the "body" would not be the Exe collection's at all.
// Editing that body would insert rules outside any collection, so refuse.
func sliceExeCollection(xml string) (before, body, after string, err error) {
	if !AppLockerDeployed(xml) {
		return "", "", "", errNoExeCollection
	}
	openLoc := exeCollectionOpens(xml)[0]
	closeRel := strings.Index(xml[openLoc[1]:], "</RuleCollection>")
	if closeRel < 0 {
		return "", "", "", fmt.Errorf("Exe rule collection is not closed")
	}
	closeAt := openLoc[1] + closeRel
	body = xml[openLoc[1]:closeAt]
	if strings.Contains(body, "<RuleCollection") {
		return "", "", "", fmt.Errorf("another rule collection starts before the Exe rule collection is closed")
	}
	return xml[:openLoc[1]], body, xml[closeAt:], nil
}

// errNoExeCollection is what sliceExeCollection reports for a document the
// agent must not edit at all: no Exe rule collection to add rules to.
var errNoExeCollection = fmt.Errorf("no AppLocker policy with an Exe rule collection; refusing to create one")

// ManagedPaths returns the Path values of the agent's own managed rules in
// xml's Exe rule collection, XML-unescaped, in document order. It returns
// nil when the document has no Exe collection, is unparseable, or simply has
// no managed rules -- there is no error return because this reads back what
// is already on disk for a status report, and a malformed document is worth
// reporting as "no paths known", not worth crashing over.
//
// Scoping is identical to RewriteAppLockerXML: only the Exe collection body
// is searched, so a rule that merely looks like ours (matching Id or Name
// prefix) but sits in another collection is never returned.
func ManagedPaths(xml string) []string {
	_, body, _, err := sliceExeCollection(xml)
	if err != nil {
		return nil
	}
	paths, _, _ := managedRulesIn(body)
	if len(paths) == 0 {
		return nil
	}
	return paths
}

// managedRulesIn reads the agent's managed rules out of one Exe collection
// body: their paths (XML-unescaped, in document order), how many managed
// rules were found, and whether every one still has the shape the agent
// writes -- Everyone/Allow with a readable FilePathCondition.
//
// The count and the intact flag exist for RewriteAppLockerXML's idempotence
// check: they are what tells "the machine already carries exactly these
// paths" from "one of our rules has been edited or cannot be read", which
// the text comparison they replaced used to catch for free.
func managedRulesIn(body string) (paths []string, count int, intact bool) {
	matches := managedRule.FindAllString(body, -1)
	intact = true
	paths = make([]string, 0, len(matches))
	for _, m := range matches {
		if !strings.Contains(m, `UserOrGroupSid="S-1-1-0"`) || !strings.Contains(m, `Action="Allow"`) {
			intact = false
			continue
		}
		sub := filePathConditionPath.FindStringSubmatch(m)
		if sub == nil {
			intact = false
			continue
		}
		paths = append(paths, html.UnescapeString(sub[1]))
	}
	return paths, len(matches), intact
}

// sameAppLockerPaths reports whether two path lists allow the same set of
// directories. Order does not matter: AppLocker unions allow rules, and a
// re-serialised policy may hand the rules back in a different order, which
// is not a reason to rewrite the machine's security policy.
func sameAppLockerPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// FilterAllowPaths re-validates the paths a published policy carries against
// model.ValidateAppLockerPath before any of them can reach RewriteAppLockerXML.
//
// policy.json is written on the publishing side -- the console, or anyone
// who can write to that OSS object, whether through a leaked admin key, a
// compromised console, or a hand-edit "just to test something". The console
// form is a usability gate, not a security boundary: this agent runs as
// SYSTEM and must not apply whatever the policy object contains without
// checking it itself, or a single bad entry (e.g. C:\Users\*) would void the
// whole AppLocker whitelist.
//
// The list is also capped at model.AppLockerMaxAllowPaths, so a hand-edited
// policy.json cannot push hundreds of rules into every machine's security
// policy.
//
// Invalid entries are dropped rather than refusing the whole list: the valid
// paths are what let an employee actually launch a tool like Codex, and one
// bad entry must not hold the rest hostage. rejected names each dropped
// entry and why, in input order, for the caller to report.
func FilterAllowPaths(paths []string) (keep []string, rejected []string) {
	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if err := model.ValidateAppLockerPath(trimmed); err != nil {
			rejected = append(rejected, fmt.Sprintf("%q: %v", p, err))
			continue
		}
		if len(keep) >= model.AppLockerMaxAllowPaths {
			rejected = append(rejected, fmt.Sprintf("%q: more than %d allow paths", p, model.AppLockerMaxAllowPaths))
			continue
		}
		// The trimmed form is what was validated, so it is the form that may
		// be written: appending the original would put a padded path -- with
		// leading whitespace -- into the rule's Path attribute.
		keep = append(keep, trimmed)
	}
	return keep, rejected
}

// rejectedErr summarises the paths FilterAllowPaths dropped, or nil when it
// dropped nothing. ApplyAppLocker returns it even when every valid path was
// applied successfully, so a dropped entry is visible on the machine's
// status line instead of silently vanishing.
func rejectedErr(rejected []string) error {
	if len(rejected) == 0 {
		return nil
	}
	return fmt.Errorf("dropped %d invalid AppLocker allow path(s): %s", len(rejected), strings.Join(rejected, "; "))
}

// RewriteAppLockerXML returns xml with the agent's managed rules replaced by
// one Everyone/Allow path rule per entry in paths, inserted at the end of the
// Exe collection. Nothing else in the document is changed, including
// EnforcementMode and rules in other rule collections.
//
// changed is decided semantically, not textually: if the Exe collection
// already allows exactly these paths, it is false and xml comes back
// untouched. It has to be. AppLocker stores rules per registry value and
// re-serialises them on read, so the newlines and indentation this function
// writes are gone by the next Get-AppLockerPolicy; comparing the rewritten
// text with the input would report a change every single cycle and rewrite
// every machine's security policy once a minute, forever.
func RewriteAppLockerXML(xml string, paths []string) (string, bool, error) {
	if !AppLockerDeployed(xml) {
		return "", false, errNoExeCollection
	}
	if strings.Count(xml, "<FilePathRule") != strings.Count(xml, "</FilePathRule>") {
		return "", false, fmt.Errorf("unbalanced FilePathRule elements; refusing to edit")
	}
	// A document with more than one Exe rule collection is not one the image
	// ever produces (it writes exactly one). Rewriting only the first, as the
	// code below does, would leave a stale managed rule with a duplicate Id
	// sitting in the second -- an invalid AppLocker policy. This is
	// unreachable against our own image, but this code edits a security
	// policy on employee machines, so refuse rather than corrupt.
	if n := len(exeCollectionOpens(xml)); n > 1 {
		return "", false, fmt.Errorf("found %d Exe rule collections; refusing to edit", n)
	}

	before, exeBody, after, err := sliceExeCollection(xml)
	if err != nil {
		return "", false, err
	}

	if current, count, intact := managedRulesIn(exeBody); intact &&
		count == len(paths) && sameAppLockerPaths(current, paths) {
		return xml, false, nil
	}

	// Only ever touch FilePathRule elements inside the Exe collection body:
	// that is the only place the agent ever writes a managed rule, and it
	// keeps this rewrite blind to any coincidental Id/Name collision
	// elsewhere in the document (see the comment on ManagedRuleIDPrefix).
	strippedBody := managedRule.ReplaceAllString(exeBody, "")

	if len(paths) > 0 {
		// Insert at the end of the collection, in the shape of the document
		// being edited. An indented document (what the image writes) ends its
		// collection body with the closing tag's own indentation on its own
		// line, so insert before that and keep the layout; the single-line
		// document Get-AppLockerPolicy returns has no layout to keep, and
		// writing indented rules into it would leave whitespace behind when
		// they are later removed. Removal must restore the document exactly.
		insertAt, indented := len(strippedBody), false
		if i := strings.LastIndex(strippedBody, "\n"); i >= 0 && strings.TrimSpace(strippedBody[i+1:]) == "" {
			insertAt, indented = i+1, true
		}
		var b strings.Builder
		for i, p := range paths {
			format := compactManagedRule
			if indented {
				format = indentedManagedRule
			}
			fmt.Fprintf(&b, format, ManagedRuleIDPrefix, i+1, ManagedRuleNamePrefix, i+1, html.EscapeString(p))
		}
		strippedBody = strippedBody[:insertAt] + b.String() + strippedBody[insertAt:]
	}

	result := before + strippedBody + after
	return result, result != xml, nil
}

// The two shapes of a managed rule. They differ only in whitespace; the
// attributes, their order and their values are identical, so a machine can
// be read and rewritten in either shape without the rule itself changing.
const (
	indentedManagedRule = "    <FilePathRule Id=\"%s%012d\" Name=\"%s%d\" Description=\"managed by ai-env-mgr policy.json appLockerAllowPaths\" UserOrGroupSid=\"S-1-1-0\" Action=\"Allow\">\n      <Conditions><FilePathCondition Path=\"%s\" /></Conditions>\n    </FilePathRule>\n"
	compactManagedRule  = "<FilePathRule Id=\"%s%012d\" Name=\"%s%d\" Description=\"managed by ai-env-mgr policy.json appLockerAllowPaths\" UserOrGroupSid=\"S-1-1-0\" Action=\"Allow\"><Conditions><FilePathCondition Path=\"%s\" /></Conditions></FilePathRule>"
)
