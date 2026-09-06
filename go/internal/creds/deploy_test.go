package creds

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func TestTargetPath_KnownEntries(t *testing.T) {
	profileDir := t.TempDir()

	cases := []struct {
		entry string
		want  string
	}{
		{model.PathCodexAuth, filepath.Join(profileDir, ".codex", "auth.json")},
		{model.PathCodexConfig, filepath.Join(profileDir, ".codex", "config.toml")},
		{model.PathClaudeCreds, filepath.Join(profileDir, ".claude", ".credentials.json")},
		{model.PathClaudeConfig, filepath.Join(profileDir, ".claude.json")},
	}

	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			got, err := TargetPath(profileDir, tc.entry)
			if err != nil {
				t.Fatalf("TargetPath(%q) unexpected error: %v", tc.entry, err)
			}
			if got != tc.want {
				t.Errorf("TargetPath(%q) = %q, want %q", tc.entry, got, tc.want)
			}
		})
	}
}

func TestTargetPath_UnknownEntry(t *testing.T) {
	profileDir := t.TempDir()

	unknownEntries := []string{
		"bogus/file.txt",
		"evil/../../etc/passwd",
		"../../etc/passwd",
		"",
		"codex/other.json",
	}

	for _, entry := range unknownEntries {
		t.Run(entry, func(t *testing.T) {
			_, err := TargetPath(profileDir, entry)
			if err == nil {
				t.Errorf("TargetPath(%q) expected error, got nil", entry)
			}
		})
	}
}

func TestWriteToProfile_WritesKnownFiles(t *testing.T) {
	profileDir := t.TempDir()

	set := model.CredentialSet{
		model.PathCodexAuth:    []byte(`{"token":"codex-auth"}`),
		model.PathCodexConfig:  []byte(`title = "codex config"`),
		model.PathClaudeCreds:  []byte(`{"token":"claude-creds"}`),
		model.PathClaudeConfig: []byte(`{"token":"claude-config"}`),
	}

	n, err := WriteToProfile(profileDir, set)
	if err != nil {
		t.Fatalf("WriteToProfile unexpected error: %v", err)
	}
	if n != 4 {
		t.Errorf("WriteToProfile returned %d, want 4", n)
	}

	for entry, want := range set {
		target, err := TargetPath(profileDir, entry)
		if err != nil {
			t.Fatalf("TargetPath(%q): %v", entry, err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("reading written file %q: %v", target, err)
		}
		if entry == model.PathClaudeConfig {
			// claude.json is merged into whatever is already there rather
			// than copied, so it comes back re-encoded. Compare meaning.
			assertSameJSON(t, got, want)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("content for %q = %q, want %q", entry, got, want)
		}
	}
}

// assertSameJSON compares two JSON documents by value, ignoring formatting.
func assertSameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("written file is not valid JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("expected value is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON differs:\n got %s\nwant %s", got, want)
	}
}

func TestWriteToProfile_SkipsUnknownEntries(t *testing.T) {
	profileDir := t.TempDir()

	set := model.CredentialSet{
		model.PathCodexAuth: []byte(`{"token":"codex-auth"}`),
		"bogus/file.txt":    []byte("should not be written"),
	}

	n, err := WriteToProfile(profileDir, set)
	if err != nil {
		t.Fatalf("WriteToProfile unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("WriteToProfile returned %d, want 1", n)
	}

	// The known entry must exist.
	target, err := TargetPath(profileDir, model.PathCodexAuth)
	if err != nil {
		t.Fatalf("TargetPath: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("expected known entry to be written: %v", err)
	}

	// The unknown entry must not exist anywhere under profileDir.
	err = filepath.Walk(profileDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Base(path) == "file.txt" {
			t.Errorf("unexpected file written for unknown entry: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking profileDir: %v", err)
	}
}

func TestWriteToProfile_FilesInCorrectSubdirectories(t *testing.T) {
	profileDir := t.TempDir()

	set := model.CredentialSet{
		model.PathCodexAuth:    []byte("a"),
		model.PathCodexConfig:  []byte("b"),
		model.PathClaudeCreds:  []byte("c"),
		model.PathClaudeConfig: []byte("d"),
	}

	if _, err := WriteToProfile(profileDir, set); err != nil {
		t.Fatalf("WriteToProfile unexpected error: %v", err)
	}

	expectedPaths := []string{
		filepath.Join(profileDir, ".codex", "auth.json"),
		filepath.Join(profileDir, ".codex", "config.toml"),
		filepath.Join(profileDir, ".claude", ".credentials.json"),
		filepath.Join(profileDir, ".claude.json"),
	}
	for _, p := range expectedPaths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file at %s: %v", p, err)
		}
	}

	// claude.json must be directly in profileDir, not under .claude/.
	if _, err := os.Stat(filepath.Join(profileDir, ".claude", "claude.json")); err == nil {
		t.Errorf(".claude.json should not be nested inside .claude/")
	}
}

func TestWriteToProfile_EmptySet(t *testing.T) {
	profileDir := t.TempDir()

	n, err := WriteToProfile(profileDir, model.CredentialSet{})
	if err != nil {
		t.Fatalf("WriteToProfile unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("WriteToProfile returned %d, want 0", n)
	}
}

// ~/.claude.json is not purely a credential: alongside the signed-in identity
// it accumulates the employee's own state. Delivering new tokens must not
// wipe their project history.
func TestClaudeConfigMergesInsteadOfReplacing(t *testing.T) {
	profileDir := t.TempDir()
	target := filepath.Join(profileDir, ".claude.json")

	existing := `{
	  "projects": {"C:\\work\\repo": {"history": ["one", "two"]}},
	  "mcpServers": {"local": {"command": "x"}},
	  "theme": "dark",
	  "hasCompletedOnboarding": false
	}`
	if err := os.WriteFile(target, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	delivered := `{"oauthAccount":{"accountUuid":"acc-1","emailAddress":"a@b.com","organizationUuid":"org-1"},"hasCompletedOnboarding":true}`
	if _, err := WriteToProfile(profileDir, model.CredentialSet{
		model.PathClaudeConfig: []byte(delivered),
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("merged file is not valid JSON: %v (%s)", err, raw)
	}

	// The employee's own state survives.
	if _, ok := got["projects"]; !ok {
		t.Error("project history was wiped")
	}
	if _, ok := got["mcpServers"]; !ok {
		t.Error("MCP server config was wiped")
	}
	if got["theme"] != "dark" {
		t.Errorf("theme = %v, want it preserved", got["theme"])
	}

	// The delivered keys win.
	if got["hasCompletedOnboarding"] != true {
		t.Error("hasCompletedOnboarding should have been set to true")
	}
	acct, ok := got["oauthAccount"].(map[string]any)
	if !ok {
		t.Fatalf("oauthAccount missing: %v", got["oauthAccount"])
	}
	if acct["accountUuid"] != "acc-1" || acct["emailAddress"] != "a@b.com" {
		t.Errorf("oauthAccount = %v, want the delivered identity", acct)
	}
}

// A machine that has never run Claude Code has no file to merge into.
func TestClaudeConfigWritesWhenAbsent(t *testing.T) {
	profileDir := t.TempDir()
	delivered := `{"hasCompletedOnboarding":true}`
	if _, err := WriteToProfile(profileDir, model.CredentialSet{
		model.PathClaudeConfig: []byte(delivered),
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(profileDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, raw, []byte(delivered))
}

// A corrupt file on the machine must not block sign-in: there is nothing
// worth preserving in unparseable JSON.
func TestClaudeConfigReplacesCorruptFile(t *testing.T) {
	profileDir := t.TempDir()
	target := filepath.Join(profileDir, ".claude.json")
	if err := os.WriteFile(target, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	delivered := `{"hasCompletedOnboarding":true}`
	if _, err := WriteToProfile(profileDir, model.CredentialSet{
		model.PathClaudeConfig: []byte(delivered),
	}); err != nil {
		t.Fatalf("a corrupt file should be replaced, not fatal: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, raw, []byte(delivered))
}

func TestWriteToProfileReportNamesSkippedEntries(t *testing.T) {
	// A silent skip is indistinguishable from success: the agent writes what
	// it knows, reports a clean deploy, and the employee is missing the file
	// the delivery existed for.
	dir := t.TempDir()
	rep, err := WriteToProfileReport(dir, model.CredentialSet{
		model.PathCodexConfig:             []byte("model = \"x\"\n"),
		"codex/from-a-newer-console.json": []byte("{}"),
		"another/unknown":                 []byte("x"),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if rep.Written != 1 {
		t.Errorf("wrote %d files, want 1", rep.Written)
	}
	if len(rep.Skipped) != 2 {
		t.Fatalf("skipped = %v, want both unknown entries", rep.Skipped)
	}
	if rep.Skipped[0] != "another/unknown" || rep.Skipped[1] != "codex/from-a-newer-console.json" {
		t.Errorf("skipped list is not sorted or wrong: %v", rep.Skipped)
	}
	// The one file written here is config.toml, which is merged -- so it is
	// tracked for presence rather than hashed.
	if len(rep.Merged) != 1 || len(rep.Placed) != 0 {
		t.Errorf("merged file should be presence-tracked: placed=%v merged=%v", rep.Placed, rep.Merged)
	}
}

func TestWriteToProfileReportSkipsNothingForKnownEntries(t *testing.T) {
	dir := t.TempDir()
	rep, err := WriteToProfileReport(dir, model.CredentialSet{
		model.PathCodexConfig: []byte("model = \"x\"\n"),
		model.PathCodexModels: []byte(`{"models":[]}`),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(rep.Skipped) != 0 {
		t.Errorf("recognised entries were reported as skipped: %v", rep.Skipped)
	}
	// config.toml is merged, so it is presence-checked rather than hashed:
	// hashing it would make an employee's own edit look like damage.
	if len(rep.Placed) != 1 || len(rep.Merged) != 1 {
		t.Errorf("verbatim vs merged split is wrong: placed=%v merged=%v", rep.Placed, rep.Merged)
	}
	if ok, drifted := VerifyPlaced(rep.Placed, rep.Merged); !ok {
		t.Errorf("freshly written files failed verification: %v", drifted)
	}
}

func TestVerifyPlacedToleratesEditsToMergedFiles(t *testing.T) {
	// config.toml is merged into whatever the employee already had, so its
	// contents are theirs. Treating an edit as damage would redeliver every
	// cycle and undo their change each time.
	dir := t.TempDir()
	rep, err := WriteToProfileReport(dir, model.CredentialSet{
		model.PathCodexConfig: []byte("model = \"grok-4.6\"\n"),
		model.PathCodexModels: []byte(`{"models":[]}`),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := filepath.Join(dir, ".codex", "config.toml")
	if err := os.WriteFile(cfg, []byte("model = \"grok-4.6\"\napproval_policy = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, drifted := VerifyPlaced(rep.Placed, rep.Merged); !ok {
		t.Errorf("an employee edit to a merged file was treated as drift: %v", drifted)
	}

	// A deleted merged file is still drift: it needs putting back.
	os.Remove(cfg)
	if ok, _ := VerifyPlaced(rep.Placed, rep.Merged); ok {
		t.Error("a deleted merged file should count as drift")
	}
}

func TestVerifyPlacedCatchesEditsToOwnedFiles(t *testing.T) {
	// models.json is written verbatim and is not the employee's to change:
	// an edited catalog would offer models the gateway refuses.
	dir := t.TempDir()
	rep, err := WriteToProfileReport(dir, model.CredentialSet{
		model.PathCodexModels: []byte(`{"models":[]}`),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	target := filepath.Join(dir, ".codex", "models.json")
	if err := os.WriteFile(target, []byte(`{"models":[{"slug":"made-up"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, drifted := VerifyPlaced(rep.Placed, rep.Merged); ok {
		t.Errorf("an edited catalog passed verification: %v", drifted)
	}
}

func TestRedeliveryOfUnchangedArchiveChangesNothing(t *testing.T) {
	dir := t.TempDir()
	set := model.CredentialSet{
		model.PathCodexAuth:   []byte(`{"token":"t1"}`),
		model.PathCodexModels: []byte(`{"models":[]}`),
	}
	first, err := WriteToProfileReport(dir, set)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if len(first.Changed) != 2 {
		t.Fatalf("first delivery should report both files as changed: %v", first.Changed)
	}

	second, err := WriteToProfileReport(dir, set)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if second.Written != 2 {
		t.Errorf("unchanged files are still placed and must count as written: %d", second.Written)
	}
	if len(second.Changed) != 0 {
		t.Errorf("identical redelivery must change nothing: %v", second.Changed)
	}
	if NeedsToolRestart(second) {
		t.Error("nothing changed, so nothing justifies killing the employee's tools")
	}
}

func TestNewCatalogWithOldLoginDoesNotRestartTools(t *testing.T) {
	// The production case: auth.json has been in the archive since the
	// employee first signed in; today's publish only adds a catalog.
	dir := t.TempDir()
	if _, err := WriteToProfileReport(dir, model.CredentialSet{
		model.PathCodexAuth: []byte(`{"token":"t1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := WriteToProfileReport(dir, model.CredentialSet{
		model.PathCodexAuth:   []byte(`{"token":"t1"}`),
		model.PathCodexModels: []byte(`{"models":[{"slug":"grok-4.6"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Changed) != 1 || rep.Changed[0] != model.PathCodexModels {
		t.Fatalf("only the catalog should register as changed: %v", rep.Changed)
	}
	if NeedsToolRestart(rep) {
		t.Error("a catalog change must not force-kill Codex")
	}
}

func TestChangedLoginRestartsTools(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteToProfileReport(dir, model.CredentialSet{
		model.PathCodexAuth: []byte(`{"token":"t1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := WriteToProfileReport(dir, model.CredentialSet{
		model.PathCodexAuth: []byte(`{"token":"t2"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !NeedsToolRestart(rep) {
		t.Error("a rotated login must restart the tools, or a revoked token keeps working")
	}
}
