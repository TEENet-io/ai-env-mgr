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
