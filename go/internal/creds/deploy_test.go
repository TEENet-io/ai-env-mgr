package creds

import (
	"os"
	"path/filepath"
	"strings"
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
		{model.PathCodexModels, filepath.Join(profileDir, ".codex", "models.json")},
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
		// Claude Code is no longer managed: an archive that still carries
		// its entries has them turned away like any other unknown path.
		"claude/.credentials.json",
		"claude.json",
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
		model.PathCodexAuth:   []byte(`{"token":"codex-auth"}`),
		model.PathCodexConfig: []byte(`title = "codex config"`),
		model.PathCodexModels: []byte(`{"models":[]}`),
	}

	n, err := WriteToProfile(profileDir, set)
	if err != nil {
		t.Fatalf("WriteToProfile unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("WriteToProfile returned %d, want 3", n)
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
		if string(got) != string(want) {
			t.Errorf("content for %q = %q, want %q", entry, got, want)
		}
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
		model.PathCodexAuth:   []byte("a"),
		model.PathCodexConfig: []byte("b"),
		model.PathCodexModels: []byte("c"),
	}

	if _, err := WriteToProfile(profileDir, set); err != nil {
		t.Fatalf("WriteToProfile unexpected error: %v", err)
	}

	expectedPaths := []string{
		filepath.Join(profileDir, ".codex", "auth.json"),
		filepath.Join(profileDir, ".codex", "config.toml"),
		filepath.Join(profileDir, ".codex", "models.json"),
	}
	for _, p := range expectedPaths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file at %s: %v", p, err)
		}
	}
}

// An earlier agent delivered a Claude Code login. Offboarding through this
// one must still take it away: a token left behind by software that no longer
// knows about it is the worst kind of leftover.
func TestRemoveSweepsTheLegacyClaudeLogin(t *testing.T) {
	profileDir := t.TempDir()
	legacy := filepath.Join(profileDir, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The employee's own Claude state is theirs and is not touched.
	own := filepath.Join(profileDir, ".claude.json")
	if err := os.WriteFile(own, []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteToProfile(profileDir, model.CredentialSet{model.PathCodexAuth: []byte("a")}); err != nil {
		t.Fatal(err)
	}

	n, err := Remove(profileDir)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n != 2 {
		t.Errorf("removed %d files, want the Codex login and the legacy Claude login", n)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("the legacy Claude login survived offboarding")
	}
	if _, err := os.Stat(own); err != nil {
		t.Error("the employee's own .claude.json was deleted")
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
}

// The restart rule on the agent side ends the employee's Codex whenever a
// delivered file's bytes moved on disk. That makes idempotence of the merged
// files a correctness property and not a nicety: a merge that reproduces the
// file with one extra blank line makes every redelivery look like a change,
// and every one of those takes somebody's work away.
//
// The employee here has edited their own config, which is the case that
// exercises the merge rather than the plain overwrite.
func TestRepeatedDeliveryToAnEditedProfileChangesNothingAfterTheFirst(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "config.toml"),
		[]byte("[tui]\ntheme = \"dark\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	set := model.CredentialSet{
		model.PathCodexAuth:   []byte(`{"token":"t1"}`),
		model.PathCodexModels: []byte(`{"models":[{"slug":"grok-4.6"}]}`),
		// The fragment the console actually renders (see
		// admincore.renderCodexConfig): the managed root keys plus the one
		// table this tool owns.
		model.PathCodexConfig: []byte("model              = \"grok-4.6\"\n" +
			"model_provider     = \"gateway\"\n" +
			"model_catalog_json = \"C:/Users/work1/.codex/models.json\"\n" +
			"web_search         = \"live\"\n" +
			"stream_idle_timeout_ms = 7200000\n\n" +
			"[model_providers.gateway]\n" +
			"name     = \"Gateway\"\n" +
			"base_url = \"https://gw.example/v1\"\n" +
			"wire_api = \"responses\"\n" +
			"experimental_bearer_token = \"sk-abc\"\n"),
	}

	first, err := WriteToProfileReport(dir, set)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if len(first.Changed) != len(set) {
		t.Fatalf("the first delivery should write every entry, changed %v", first.Changed)
	}

	// Three more deliveries of the same archive. Nothing may move, and in
	// particular not on the second pass: a merge that is stable only from the
	// third pass on would still have cost every employee one interruption.
	for pass := 2; pass <= 4; pass++ {
		rep, err := WriteToProfileReport(dir, set)
		if err != nil {
			t.Fatalf("delivery %d: %v", pass, err)
		}
		if rep.Written != len(set) {
			t.Errorf("delivery %d wrote %d entries, want %d", pass, rep.Written, len(set))
		}
		if len(rep.Changed) != 0 {
			t.Errorf("delivery %d changed %v; redelivering the same archive must move nothing",
				pass, rep.Changed)
		}
	}

	// The employee's own setting survived all of it -- an idempotent merge
	// that idempotently discards their edits would pass the check above.
	got, err := os.ReadFile(filepath.Join(dir, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[tui]", `theme = "dark"`, "[model_providers.gateway]", "base_url"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config.toml lost %q after four deliveries:\n%s", want, got)
		}
	}
}

// Codex writes the employee's model choice into config.toml. A delivery
// keeps it while the catalog still offers it, and ends the running Codex
// only when the token or the gateway changed.
func TestADeliveryKeepsTheEmployeesModelAndStopsCodexOnlyForNewCredentials(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".codex", "config.toml")
	fragment := func(token string) []byte {
		return []byte("model              = \"gpt-5\"\n" +
			"model_provider     = \"gateway\"\n" +
			"model_catalog_json = \"C:/Users/work1/.codex/models.json\"\n\n" +
			"[model_providers.gateway]\n" +
			"name     = \"Gateway\"\n" +
			"base_url = \"https://gw.example/v1\"\n" +
			"experimental_bearer_token = \"" + token + "\"\n")
	}
	catalog := func(slugs ...string) []byte {
		var parts []string
		for _, s := range slugs {
			parts = append(parts, `{"slug":"`+s+`"}`)
		}
		return []byte(`{"models":[` + strings.Join(parts, ",") + `]}`)
	}
	deliver := func(token string, slugs ...string) Report {
		t.Helper()
		rep, err := WriteToProfileReport(dir, model.CredentialSet{
			model.PathCodexConfig: fragment(token), model.PathCodexModels: catalog(slugs...)})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	modelIn := func() string {
		data, _ := os.ReadFile(cfgPath)
		v, _ := rootStringValue(data, "model")
		return v
	}

	if rep := deliver("sk-1", "gpt-5", "claude-5"); !rep.CredentialChanged || modelIn() != "gpt-5" {
		t.Fatalf("first delivery: credential changed %v, model %q", rep.CredentialChanged, modelIn())
	}
	// The employee picks another model in Codex, which writes it back.
	data, _ := os.ReadFile(cfgPath)
	edited, _ := setRootString(data, "model", "claude-5")
	os.WriteFile(cfgPath, append(edited, []byte("\n[projects.'C:/work']\ntrust_level = \"trusted\"\n")...), 0o600)

	// A new catalog (a model added): the choice stays, Codex keeps running.
	rep := deliver("sk-1", "gpt-5", "claude-5", "gemini-3")
	if modelIn() != "claude-5" || rep.CredentialChanged {
		t.Fatalf("catalog change: model %q, credential changed %v", modelIn(), rep.CredentialChanged)
	}
	if again := deliver("sk-1", "gpt-5", "claude-5", "gemini-3"); len(again.Changed) != 0 {
		t.Fatalf("the same package again moved %v", again.Changed)
	}
	if got, _ := os.ReadFile(cfgPath); !strings.Contains(string(got), "trust_level") {
		t.Fatal("Codex's own settings must survive a delivery")
	}
	// A new token: Codex has to go.
	if rep := deliver("sk-2", "gpt-5", "claude-5", "gemini-3"); !rep.CredentialChanged || modelIn() != "claude-5" {
		t.Fatalf("token change: credential changed %v, model %q", rep.CredentialChanged, modelIn())
	}
	// The chosen model is taken off the catalog: back to the default.
	if rep := deliver("sk-2", "gpt-5", "gemini-3"); modelIn() != "gpt-5" || rep.CredentialChanged {
		t.Fatalf("model removed: model %q, credential changed %v", modelIn(), rep.CredentialChanged)
	}
}
