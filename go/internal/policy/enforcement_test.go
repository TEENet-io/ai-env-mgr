package policy

import (
	"regexp"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// collectionModes reads the EnforcementMode of every rule collection, keyed by
// its Type, without reusing the code under test to find them.
func collectionModes(t *testing.T, doc string) map[string]string {
	t.Helper()
	tag := regexp.MustCompile(`<RuleCollection\s[^>]*>`)
	attr := func(tagText, name string) string {
		m := regexp.MustCompile(name + `="([^"]*)"`).FindStringSubmatch(tagText)
		if m == nil {
			t.Fatalf("no %s attribute in %q", name, tagText)
		}
		return m[1]
	}
	out := map[string]string{}
	for _, tagText := range tag.FindAllString(doc, -1) {
		out[attr(tagText, "Type")] = attr(tagText, "EnforcementMode")
	}
	if len(out) == 0 {
		t.Fatal("no rule collections in document")
	}
	return out
}

// anyEnforcementMode blanks out every EnforcementMode value, so two documents
// can be compared on every byte the switch is not allowed to touch.
var anyEnforcementMode = regexp.MustCompile(`EnforcementMode="[^"]*"`)

func normaliseModes(doc string) string {
	return anyEnforcementMode.ReplaceAllString(doc, `EnforcementMode="?"`)
}

// The unmanaged default must reach the XML layer as "do nothing at all",
// not as "enforce": every policy object in the field carries no mode.
func TestSetEnforcementModeUnmanagedIsANoOp(t *testing.T) {
	for name, doc := range bothShapes(t) {
		out, changed, err := SetEnforcementMode(doc, "")
		if err != nil || changed || out != doc {
			t.Errorf("%s: SetEnforcementMode(doc, \"\") = (changed %v, err %v); document %s",
				name, changed, err, map[bool]string{true: "rewritten", false: "untouched"}[out != doc])
		}
	}
}

func TestSetEnforcementModeSwitchesConfiguredCollectionsBothWays(t *testing.T) {
	for name, doc := range bothShapes(t) {
		audited, changed, err := SetEnforcementMode(doc, model.AppLockerModeAudit)
		if err != nil {
			t.Fatalf("%s: to audit: %v", name, err)
		}
		if !changed {
			t.Fatalf("%s: switching an enforced policy to audit reported no change", name)
		}
		for typ, mode := range collectionModes(t, audited) {
			want := "AuditOnly"
			if collectionModes(t, doc)[typ] == "NotConfigured" {
				want = "NotConfigured"
			}
			if mode != want {
				t.Errorf("%s: %s collection is %s, want %s", name, typ, mode, want)
			}
		}

		back, changed, err := SetEnforcementMode(audited, model.AppLockerModeEnforce)
		if err != nil {
			t.Fatalf("%s: back to enforce: %v", name, err)
		}
		if !changed {
			t.Fatalf("%s: switching an audited policy back to enforce reported no change", name)
		}
		// Reversible to the byte: this is the whole point of using audit mode
		// rather than removing rules.
		if back != doc {
			t.Errorf("%s: enforce -> audit -> enforce did not restore the document", name)
		}
	}
}

// A NotConfigured collection holds no rules. Enforcing or auditing it would
// be a change to the machine's policy that nobody asked for.
func TestSetEnforcementModeLeavesUnconfiguredCollectionsAlone(t *testing.T) {
	doc := noPolicyFixture(t)
	for _, mode := range []string{model.AppLockerModeAudit, model.AppLockerModeEnforce} {
		out, changed, err := SetEnforcementMode(doc, mode)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if changed || out != doc {
			t.Errorf("%s: a policy with only NotConfigured collections must be left alone; got %q", mode, out)
		}
	}
}

// The agent calls this on every sync cycle. A machine already in the target
// mode must not have its security policy rewritten once a minute, forever.
func TestSetEnforcementModeIsIdempotent(t *testing.T) {
	for name, doc := range bothShapes(t) {
		for _, mode := range []string{model.AppLockerModeAudit, model.AppLockerModeEnforce} {
			once, _, err := SetEnforcementMode(doc, mode)
			if err != nil {
				t.Fatal(err)
			}
			twice, changed, err := SetEnforcementMode(once, mode)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if changed || twice != once {
				t.Errorf("%s: a second switch to %s reported changed=%v", name, mode, changed)
			}
		}
	}
}

// Everything outside the EnforcementMode attributes must survive byte for
// byte: no rule added, removed, reordered or reindented.
func TestSetEnforcementModeTouchesNothingElse(t *testing.T) {
	for name, doc := range bothShapes(t) {
		out, _, err := SetEnforcementMode(doc, model.AppLockerModeAudit)
		if err != nil {
			t.Fatal(err)
		}
		if normaliseModes(out) != normaliseModes(doc) {
			t.Errorf("%s: the document changed outside its EnforcementMode attributes", name)
		}
		if strings.Count(out, "<FilePathRule") != strings.Count(doc, "<FilePathRule") {
			t.Errorf("%s: the number of rules changed", name)
		}
	}
}

// Attribute order is not guaranteed by anything, and a hand-written or
// re-serialised policy may put EnforcementMode first.
func TestSetEnforcementModeHandlesAttributeOrder(t *testing.T) {
	doc := `<AppLockerPolicy Version="1"><RuleCollection EnforcementMode="Enabled" Type="Exe">` +
		`<FilePathRule Id="a" Name="n" Description="" UserOrGroupSid="S-1-1-0" Action="Allow">` +
		`<Conditions><FilePathCondition Path="*" /></Conditions></FilePathRule></RuleCollection>` +
		`<RuleCollection EnforcementMode="NotConfigured" Type="Msi" /></AppLockerPolicy>`
	out, changed, err := SetEnforcementMode(doc, model.AppLockerModeAudit)
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if !strings.Contains(out, `<RuleCollection EnforcementMode="AuditOnly" Type="Exe">`) {
		t.Errorf("Exe collection not switched: %s", out)
	}
	if !strings.Contains(out, `<RuleCollection EnforcementMode="NotConfigured" Type="Msi" />`) {
		t.Errorf("the self-closing NotConfigured collection must be untouched: %s", out)
	}
}

func TestSetEnforcementModeRefusesForeignDocuments(t *testing.T) {
	for _, doc := range []string{"", "not xml at all", `<Foo><RuleCollection Type="Exe" EnforcementMode="Enabled"></RuleCollection></Foo>`} {
		if _, _, err := SetEnforcementMode(doc, model.AppLockerModeAudit); err == nil {
			t.Errorf("a document that is not an AppLocker policy must be refused: %q", doc)
		}
	}
}

// An unknown mode must never be turned into an EnforcementMode value on a
// guess: policy.json is not a trust boundary (see FilterAllowPaths).
func TestSetEnforcementModeRefusesAnUnknownMode(t *testing.T) {
	if _, _, err := SetEnforcementMode(fixture(t), "Enabled"); err == nil {
		t.Error("an unknown mode must be refused")
	}
}
