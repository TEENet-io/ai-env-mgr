package policy

import (
	"os"
	"reflect"
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
	if !strings.Contains(exe, `Id="e0000000-0000-0000-0000-000000000001" Name="AIEnvMgr-allow-1"`) ||
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

func TestRewriteLeavesTheImagesMsiRuleAlone(t *testing.T) {
	out, _, err := RewriteAppLockerXML(fixture(t), []string{`C:\tools\Codex\*`})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(out, `Id="c0000000-0000-0000-0000-000000000001" Name="Admins-allow-all-msi"`) {
		t.Errorf("image's Msi rule (and its original id) must survive untouched:\n%s", out)
	}
	msi := out[strings.Index(out, `<RuleCollection Type="Msi"`):strings.Index(out, `<RuleCollection Type="Appx"`)]
	if strings.Contains(msi, ManagedRuleNamePrefix) {
		t.Errorf("managed rules must not leak into the Msi collection:\n%s", msi)
	}
}

func TestRewriteEscapesXMLInPaths(t *testing.T) {
	out, _, err := RewriteAppLockerXML(fixture(t), []string{`C:\tools\A&B\*`})
	if err != nil || !strings.Contains(out, `Path="C:\tools\A&amp;B\*"`) {
		t.Errorf("ampersand must be escaped: %v\n%s", err, out)
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
	out, _, err := RewriteAppLockerXML(fixture(t), []string{`C:\tools\Codex\*`, `C:\tools\A&B\*`})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	got := ManagedPaths(out)
	want := []string{`C:\tools\Codex\*`, `C:\tools\A&B\*`}
	if len(got) != len(want) {
		t.Fatalf("ManagedPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestManagedPathsNilWhenNoneManaged(t *testing.T) {
	if got := ManagedPaths(fixture(t)); got != nil {
		t.Errorf("expected nil for the plain image XML (no managed rules), got %v", got)
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

// A rule that merely matches the managed Id/Name prefixes but sits in a
// different rule collection must not be reported: ManagedPaths is scoped to
// the Exe collection exactly like RewriteAppLockerXML.
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
