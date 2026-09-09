// Package policy's AppLocker support rewrites the machine's local AppLocker
// XML to add or remove rules the agent owns, without disturbing anything
// else in the document -- including the image's own rules and
// EnforcementMode. This file is pure string/XML manipulation; it does not
// touch the registry or any Windows API (see registry_windows.go for that).
package policy

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

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
// EnforcementMode and rules in other rule collections. changed is false
// when the result equals the input.
func RewriteAppLockerXML(xml string, paths []string) (string, bool, error) {
	if !AppLockerDeployed(xml) {
		return "", false, fmt.Errorf("no AppLocker policy with an Exe rule collection; refusing to create one")
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
	if n := len(exeCollectionOpen.FindAllStringIndex(xml, -1)); n > 1 {
		return "", false, fmt.Errorf("found %d Exe rule collections; refusing to edit", n)
	}

	openLoc := exeCollectionOpen.FindStringIndex(xml)
	closeRel := strings.Index(xml[openLoc[1]:], "</RuleCollection>")
	if closeRel < 0 {
		return "", false, fmt.Errorf("Exe rule collection is not closed")
	}
	closeAt := openLoc[1] + closeRel

	before := xml[:openLoc[1]]
	exeBody := xml[openLoc[1]:closeAt]
	after := xml[closeAt:]

	// Only ever touch FilePathRule elements inside the Exe collection body:
	// that is the only place the agent ever writes a managed rule, and it
	// keeps this rewrite blind to any coincidental Id/Name collision
	// elsewhere in the document (see the comment on ManagedRuleIDPrefix).
	strippedBody := managedRule.ReplaceAllString(exeBody, "")

	if len(paths) > 0 {
		var b strings.Builder
		for i, p := range paths {
			fmt.Fprintf(&b, "    <FilePathRule Id=\"%s%012d\" Name=\"%s%d\" Description=\"managed by ai-env-mgr policy.json appLockerAllowPaths\" UserOrGroupSid=\"S-1-1-0\" Action=\"Allow\">\n      <Conditions><FilePathCondition Path=\"%s\" /></Conditions>\n    </FilePathRule>\n",
				ManagedRuleIDPrefix, i+1, ManagedRuleNamePrefix, i+1, html.EscapeString(p))
		}
		// Insert before the indentation of the closing tag so the result
		// keeps the image's layout.
		lineStart := strings.LastIndex(strippedBody, "\n") + 1
		strippedBody = strippedBody[:lineStart] + b.String() + strippedBody[lineStart:]
	}

	result := before + strippedBody + after
	return result, result != xml, nil
}
