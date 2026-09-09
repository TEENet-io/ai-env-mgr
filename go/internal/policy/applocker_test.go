package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Two shapes of the same policy, and they are not interchangeable:
//
//   - applocker_set_input_image.xml is what the image script feeds to
//     Set-AppLockerPolicy -- indented, four rule collections.
//   - applocker_get_output_imaged.xml is what Get-AppLockerPolicy -Local -Xml
//     hands back for the same machine, which is what this code actually
//     receives: one line, no indentation, all five collections in
//     alphabetical order, the unconfigured one self-closing.
//   - applocker_get_output_no_policy.xml is that same read on a machine with
//     no AppLocker at all: five self-closing NotConfigured collections.
//
// The rewriter must behave identically on the first two.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fixture(t *testing.T) string { return readFixture(t, "applocker_set_input_image.xml") }

func getShapeFixture(t *testing.T) string { return readFixture(t, "applocker_get_output_imaged.xml") }

func noPolicyFixture(t *testing.T) string {
	return readFixture(t, "applocker_get_output_no_policy.xml")
}

// bothShapes is the imaged machine as written and as read back.
func bothShapes(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"set-input (indented)":  fixture(t),
		"get-output (one line)": getShapeFixture(t),
	}
}

// exeCollection returns the Exe rule collection element of doc, without
// reusing the code under test to find it.
func exeCollection(t *testing.T, doc string) string {
	t.Helper()
	start := strings.Index(doc, `<RuleCollection Type="Exe"`)
	if start < 0 {
		t.Fatal("no Exe collection in document")
	}
	end := strings.Index(doc[start:], `</RuleCollection>`)
	if end < 0 {
		t.Fatal("Exe collection is not closed")
	}
	return doc[start : start+end]
}

// reserialise mimics what AppLocker does to a policy it has stored: the rules
// come back on one line, without the whitespace the agent wrote.
var interTagWhitespace = regexp.MustCompile(`>\s+<`)

func reserialise(doc string) string {
	return strings.TrimSpace(interTagWhitespace.ReplaceAllString(doc, "><"))
}

func TestRewriteAddsManagedRulesToExeCollectionOnly(t *testing.T) {
	for name, fx := range bothShapes(t) {
		t.Run(name, func(t *testing.T) {
			out, changed, err := RewriteAppLockerXML(fx, []string{`C:\tools\Codex\*`, `D:\Apps\Foo\*`})
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			exe := exeCollection(t, out)
			if strings.Count(exe, ManagedRuleNamePrefix) != 2 {
				t.Errorf("expected 2 managed rules in Exe collection:\n%s", exe)
			}
			if !strings.Contains(exe, `Id="e0000000-0000-0000-0000-000000000001" Name="AIEnvMgr-allow-1"`) ||
				!strings.Contains(exe, `<FilePathCondition Path="C:\tools\Codex\*" />`) ||
				!strings.Contains(exe, `UserOrGroupSid="S-1-1-0" Action="Allow"`) {
				t.Errorf("managed rule shape wrong:\n%s", exe)
			}
			if rest := strings.Replace(out, exe, "", 1); strings.Contains(rest, ManagedRuleNamePrefix) {
				t.Error("managed rules must not leak into other collections")
			}
			// Everything the image wrote is still there, byte for byte.
			for _, keep := range []string{`Admins-allow-all`, `Everyone-allow-Windows`, `Everyone-allow-ProgramFiles`, `EnforcementMode="Enabled"`, `%SYSTEM32%\reg.exe`} {
				if strings.Count(out, keep) != strings.Count(fx, keep) {
					t.Errorf("image rule %q was altered", keep)
				}
			}
		})
	}
}

func TestRewriteIsIdempotentAndReplacesOldManagedRules(t *testing.T) {
	for name, fx := range bothShapes(t) {
		t.Run(name, func(t *testing.T) {
			once, _, _ := RewriteAppLockerXML(fx, []string{`C:\tools\Codex\*`})
			twice, changed, err := RewriteAppLockerXML(once, []string{`C:\tools\Codex\*`})
			if err != nil || changed || twice != once {
				t.Fatalf("second pass must be a no-op: changed=%v err=%v", changed, err)
			}
			replaced, changed, _ := RewriteAppLockerXML(once, []string{`D:\Apps\Foo\*`})
			if !changed || strings.Contains(replaced, `C:\tools\Codex`) || strings.Count(replaced, ManagedRuleNamePrefix) != 1 {
				t.Errorf("old managed rule must be replaced, not accumulated:\n%s", replaced)
			}
		})
	}
}

// The rule the agent inserts carries its own newlines and indentation, and
// AppLocker throws that away when it stores and re-serialises the policy. A
// textual "did the document change?" therefore answered yes forever: one
// Set-AppLockerPolicy per machine per minute, permanent policy churn, and the
// staging window reopened 1440 times a day. The decision has to be semantic.
func TestRewriteIsIdempotentAcrossAReserialisedRoundTrip(t *testing.T) {
	for name, fx := range bothShapes(t) {
		t.Run(name, func(t *testing.T) {
			paths := []string{`C:\tools\Codex\*`, `D:\Apps\Foo\*`}
			once, changed, err := RewriteAppLockerXML(fx, paths)
			if err != nil || !changed {
				t.Fatalf("first pass: changed=%v err=%v", changed, err)
			}
			back := reserialise(once)
			if _, changed, err := RewriteAppLockerXML(back, paths); err != nil || changed {
				t.Errorf("after a re-serialised round trip: changed=%v err=%v (want false)", changed, err)
			}
		})
	}
}

// AppLocker may hand the rules back in a different order than they went in;
// a set of allow rules is the same policy either way and is not worth a write.
func TestRewriteIgnoresManagedRuleOrder(t *testing.T) {
	fx := getShapeFixture(t)
	once, _, _ := RewriteAppLockerXML(fx, []string{`C:\tools\Codex\*`, `D:\Apps\Foo\*`})
	if _, changed, err := RewriteAppLockerXML(once, []string{`D:\Apps\Foo\*`, `C:\tools\Codex\*`}); err != nil || changed {
		t.Errorf("same paths in another order: changed=%v err=%v (want false)", changed, err)
	}
}

// Deciding semantically must not stop the agent repairing one of its own
// rules that has been edited on the machine.
func TestRewriteRepairsAnEditedManagedRule(t *testing.T) {
	fx := getShapeFixture(t)
	once, _, _ := RewriteAppLockerXML(fx, []string{`C:\tools\Codex\*`})
	// Tamper inside the managed rule, not in one of the image's own.
	at := strings.Index(once, ManagedRuleNamePrefix)
	tampered := once[:at] + strings.Replace(once[at:], `UserOrGroupSid="S-1-1-0"`, `UserOrGroupSid="S-1-5-32-544"`, 1)
	if tampered == once {
		t.Fatal("test setup broken: nothing was tampered with")
	}
	out, changed, err := RewriteAppLockerXML(tampered, []string{`C:\tools\Codex\*`})
	if err != nil || !changed {
		t.Fatalf("an edited managed rule must be rewritten: changed=%v err=%v", changed, err)
	}
	if out != once {
		t.Errorf("repair should restore the agent's own rule:\n got %s\nwant %s", exeCollection(t, out), exeCollection(t, once))
	}
}

func TestRewriteWithNoPathsRemovesManagedRules(t *testing.T) {
	for name, fx := range bothShapes(t) {
		t.Run(name, func(t *testing.T) {
			once, _, _ := RewriteAppLockerXML(fx, []string{`C:\tools\Codex\*`})
			back, changed, err := RewriteAppLockerXML(once, nil)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if back != fx {
				t.Errorf("removing all managed rules must restore the document exactly:\n got %q\nwant %q", back, fx)
			}
			same, changed, _ := RewriteAppLockerXML(fx, nil)
			if changed || same != fx {
				t.Error("no managed rules and no paths must be a no-op")
			}
		})
	}
}

func TestRewriteRefusesForeignOrMissingPolicy(t *testing.T) {
	if _, _, err := RewriteAppLockerXML(``, []string{`C:\x\*`}); err == nil {
		t.Error("empty document must be refused")
	}
	if _, _, err := RewriteAppLockerXML(`<AppLockerPolicy Version="1"></AppLockerPolicy>`, []string{`C:\x\*`}); err == nil {
		t.Error("policy without an Exe collection must be refused, not created")
	}
	if AppLockerDeployed(`<AppLockerPolicy Version="1"></AppLockerPolicy>`) || !AppLockerDeployed(fixture(t)) ||
		!AppLockerDeployed(getShapeFixture(t)) {
		t.Error("AppLockerDeployed wrong")
	}
}

// Get-AppLockerPolicy always returns all five rule collections; the ones that
// were never configured come back self-closing. A machine with no AppLocker
// at all must read as "not deployed", not as a deployed policy whose Exe
// collection is somehow unclosed -- that reported a hard error on every sync
// cycle even when nothing was published.
func TestSelfClosingExeCollectionIsNotDeployed(t *testing.T) {
	none := noPolicyFixture(t)
	if AppLockerDeployed(none) {
		t.Error("a machine with all collections NotConfigured must not read as deployed")
	}
	if got := ManagedPaths(none); got != nil {
		t.Errorf("ManagedPaths on an undeployed machine = %v, want nil", got)
	}
	for _, paths := range [][]string{{`C:\tools\Codex\*`}, nil} {
		out, changed, err := RewriteAppLockerXML(none, paths)
		if err == nil {
			t.Errorf("paths=%v: an undeployed machine must be refused, got changed=%v", paths, changed)
		}
		if out != "" {
			t.Errorf("paths=%v: expected no output on refusal, got %q", paths, out)
		}
	}
}

// The Exe collection self-closing while a later collection is populated used
// to slice the *Msi* collection's closing tag as the Exe one's, and insert
// the agent's rules outside every collection -- schema-invalid XML, written
// straight back into the machine's security policy.
func TestRewriteRefusesWhenAnotherCollectionOpensFirst(t *testing.T) {
	mixed := strings.Replace(noPolicyFixture(t),
		`<RuleCollection Type="Msi" EnforcementMode="NotConfigured" />`,
		`<RuleCollection Type="Msi" EnforcementMode="Enabled"><FilePathRule Id="c0000000-0000-0000-0000-000000000001" Name="Admins-allow-all-msi" Description="" UserOrGroupSid="S-1-5-32-544" Action="Allow"><Conditions><FilePathCondition Path="*" /></Conditions></FilePathRule></RuleCollection>`, 1)
	out, changed, err := RewriteAppLockerXML(mixed, []string{`C:\tools\Codex\*`})
	if err == nil {
		t.Fatalf("expected a refusal, got changed=%v out=%s", changed, out)
	}
	if out != "" {
		t.Errorf("expected no output on refusal, got %q", out)
	}

	// Same shape, but with the Exe collection genuinely open and unclosed.
	unclosed := `<AppLockerPolicy Version="1"><RuleCollection Type="Exe" EnforcementMode="Enabled"><RuleCollection Type="Msi" EnforcementMode="Enabled"></RuleCollection></AppLockerPolicy>`
	if _, _, err := RewriteAppLockerXML(unclosed, []string{`C:\tools\Codex\*`}); err == nil {
		t.Error("a nested rule collection must be refused")
	}
}

func TestRewriteLeavesTheImagesMsiRuleAlone(t *testing.T) {
	for name, fx := range bothShapes(t) {
		t.Run(name, func(t *testing.T) {
			out, _, err := RewriteAppLockerXML(fx, []string{`C:\tools\Codex\*`})
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if !strings.Contains(out, `Id="c0000000-0000-0000-0000-000000000001" Name="Admins-allow-all-msi"`) {
				t.Errorf("image's Msi rule (and its original id) must survive untouched:\n%s", out)
			}
			msi := out[strings.Index(out, `<RuleCollection Type="Msi"`):]
			msi = msi[:strings.Index(msi, `</RuleCollection>`)]
			if strings.Contains(msi, ManagedRuleNamePrefix) {
				t.Errorf("managed rules must not leak into the Msi collection:\n%s", msi)
			}
		})
	}
}

func TestRewriteEscapesXMLInPaths(t *testing.T) {
	for name, fx := range bothShapes(t) {
		t.Run(name, func(t *testing.T) {
			out, _, err := RewriteAppLockerXML(fx, []string{`C:\tools\A&B\*`})
			if err != nil || !strings.Contains(out, `Path="C:\tools\A&amp;B\*"`) {
				t.Errorf("ampersand must be escaped: %v\n%s", err, out)
			}
		})
	}
}

func TestRewriteRefusesTwoExeCollections(t *testing.T) {
	fx := fixture(t)
	start := strings.Index(fx, `<RuleCollection Type="Exe"`)
	end := strings.Index(fx, `</RuleCollection>`) + len(`</RuleCollection>`)
	exeCollection := fx[start:end]
	doc := `<AppLockerPolicy Version="1">` + exeCollection + exeCollection + `</AppLockerPolicy>`

	out, changed, err := RewriteAppLockerXML(doc, []string{`C:\tools\Codex\*`})
	if err == nil {
		t.Fatalf("expected an error for a document with two Exe rule collections, got changed=%v out=%q", changed, out)
	}
	if out != "" {
		t.Errorf("expected no output on refusal, got %q", out)
	}
}

func TestManagedPathsReturnsInOrderAndUnescaped(t *testing.T) {
	for name, fx := range bothShapes(t) {
		t.Run(name, func(t *testing.T) {
			out, _, err := RewriteAppLockerXML(fx, []string{`C:\tools\Codex\*`, `C:\tools\A&B\*`})
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			got := ManagedPaths(out)
			want := []string{`C:\tools\Codex\*`, `C:\tools\A&B\*`}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ManagedPaths = %v, want %v", got, want)
			}
		})
	}
}

func TestManagedPathsNilWhenNoneManaged(t *testing.T) {
	for name, fx := range bothShapes(t) {
		t.Run(name, func(t *testing.T) {
			if got := ManagedPaths(fx); got != nil {
				t.Errorf("expected nil for the plain image XML (no managed rules), got %v", got)
			}
		})
	}
}

func TestManagedPathsNilWithoutExeCollection(t *testing.T) {
	if got := ManagedPaths(`<AppLockerPolicy Version="1"></AppLockerPolicy>`); got != nil {
		t.Errorf("expected nil without an Exe collection, got %v", got)
	}
	if got := ManagedPaths(``); got != nil {
		t.Errorf("expected nil for an empty/malformed document, got %v", got)
	}
}

// FilterAllowPaths is the agent-side re-validation of whatever policy.json
// carried: the console form is a usability gate, not a security boundary,
// so the enforcing side must not trust it blindly.

func TestFilterAllowPathsDropsInvalidAndNamesThem(t *testing.T) {
	keep, rejected := FilterAllowPaths([]string{
		`C:\tools\Codex\*`, `C:\Users\Evil\*`, `D:\Apps\Foo\*`, `%LOCALAPPDATA%\x\*`,
	})
	want := []string{`C:\tools\Codex\*`, `D:\Apps\Foo\*`}
	if !reflect.DeepEqual(keep, want) {
		t.Errorf("keep = %v, want %v", keep, want)
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected = %v, want 2 entries", rejected)
	}
	// %q-quoting escapes the backslashes, so look for a substring that
	// survives that rather than the raw path.
	for _, want := range []string{`Users`, `LOCALAPPDATA`} {
		found := false
		for _, r := range rejected {
			if strings.Contains(r, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("rejected = %v, none names the dropped path containing %q", rejected, want)
		}
	}
}

func TestFilterAllowPathsAllInvalidKeepsNoneNotAFreeForAll(t *testing.T) {
	keep, rejected := FilterAllowPaths([]string{`C:\Users\*`, `%TEMP%\evil\*`})
	if len(keep) != 0 {
		t.Errorf("keep = %v, want none", keep)
	}
	if len(rejected) != 2 {
		t.Errorf("rejected = %v, want 2", rejected)
	}
	// Feeding the filtered result to the rewriter must land as "no managed
	// rules", not smuggle the invalid paths through as an allow-everything
	// rule.
	out, _, err := RewriteAppLockerXML(fixture(t), keep)
	if err != nil {
		t.Fatal(err)
	}
	if got := ManagedPaths(out); got != nil {
		t.Errorf("expected no managed rules from an all-invalid list, got %v", got)
	}
}

func TestFilterAllowPathsAllValidKeepsEverything(t *testing.T) {
	in := []string{`C:\tools\Codex\*`, `D:\Apps\Foo\*`}
	keep, rejected := FilterAllowPaths(in)
	if !reflect.DeepEqual(keep, in) {
		t.Errorf("keep = %v, want %v", keep, in)
	}
	if len(rejected) != 0 {
		t.Errorf("rejected = %v, want none", rejected)
	}
}

// A rule that merely matches the managed Id/Name prefixes but sits in a
// different rule collection must not be reported: ManagedPaths is scoped to
// the Exe collection exactly like RewriteAppLockerXML.
func TestManagedPathsIgnoresAManagedLookingRuleInAnotherCollection(t *testing.T) {
	fx := fixture(t)
	planted := `    <FilePathRule Id="e0000000-0000-0000-0000-000000000099" Name="AIEnvMgr-allow-99" Description="planted" UserOrGroupSid="S-1-1-0" Action="Allow">
      <Conditions><FilePathCondition Path="C:\should\not\appear\*" /></Conditions>
    </FilePathRule>
`
	marker := `<RuleCollection Type="Msi" EnforcementMode="Enabled">`
	doc := strings.Replace(fx, marker, marker+"\n"+planted, 1)
	if !strings.Contains(doc, "should\\not\\appear") {
		t.Fatal("test setup broken: planted rule not found in the Msi collection")
	}
	if got := ManagedPaths(doc); got != nil {
		t.Errorf("a managed-looking rule outside the Exe collection must not be returned, got %v", got)
	}
}

// The policy XML must never be staged in a temp directory: for a service
// running as LocalSystem os.TempDir() is C:\Windows\Temp, which a standard
// user can write to, and SYSTEM then hands that file to Set-AppLockerPolicy.
func TestAppLockerStagingDirIsNeverATempDirectory(t *testing.T) {
	t.Cleanup(func() { SetAppLockerStagingDir("") })

	SetAppLockerStagingDir("")
	t.Setenv("ProgramData", `C:\ProgramData`)
	got, err := appLockerStagingDir()
	if err != nil || got != filepath.Join(`C:\ProgramData`, "AIEnvMgr") {
		t.Errorf("default staging dir = %q, %v; want %%ProgramData%%\\AIEnvMgr", got, err)
	}
	if strings.EqualFold(got, os.TempDir()) {
		t.Errorf("staging dir must not be the temp directory, got %q", got)
	}

	SetAppLockerStagingDir(`C:\ProgramData\AIEnvMgr`)
	if got, err := appLockerStagingDir(); err != nil || got != `C:\ProgramData\AIEnvMgr` {
		t.Errorf("configured staging dir = %q, %v", got, err)
	}

	SetAppLockerStagingDir("")
	t.Setenv("ProgramData", "")
	if got, err := appLockerStagingDir(); err == nil {
		t.Errorf("with no configured directory and no %%ProgramData%%, want an error, got %q", got)
	}
}
