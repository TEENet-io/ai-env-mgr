package creds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const deliveredConfig = `model              = "grok-4.6"
model_provider     = "gateway"
model_catalog_json = "C:/Users/alice/.codex/models.json"
web_search         = "live"

[model_providers.gateway]
name     = "Gateway"
base_url = "https://litellm.teenet.app/v1"
wire_api = "responses"
`

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestMergeCodexConfig_NoExistingFileWritesDelivered(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "config.toml")
	got, err := mergeCodexConfig(missing, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if string(got) != deliveredConfig {
		t.Fatalf("expected delivered config verbatim, got:\n%s", got)
	}
}

func TestMergeCodexConfig_KeepsEmployeeSettings(t *testing.T) {
	target := writeTemp(t, `# my own notes
approval_policy = "on-request"
sandbox_mode    = "workspace-write"

[mcp_servers.mine]
command = "node"
args    = ["server.js"]
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	for _, want := range []string{
		"# my own notes",
		`approval_policy = "on-request"`,
		`sandbox_mode    = "workspace-write"`,
		"[mcp_servers.mine]",
		`command = "node"`,
		`args    = ["server.js"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("employee setting %q was dropped:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "[model_providers.gateway]") {
		t.Errorf("gateway provider missing:\n%s", out)
	}
}

func TestMergeCodexConfig_ReplacesManagedKeysWithoutDuplicating(t *testing.T) {
	target := writeTemp(t, `model          = "gpt-5.6-sol"
model_provider = "openai"
approval_policy = "never"

[model_providers.gateway]
base_url = "https://old-gateway.example/v1"
wire_api = "chat"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	// A repeated key makes TOML fail to load outright, so this is the
	// property that actually matters. Count assignments at the root, not
	// substrings: "model_provider" also occurs inside the table header
	// "[model_providers.gateway]".
	for _, key := range []string{"model", "model_provider", "model_catalog_json"} {
		if n := countRootAssignments(out, key); n != 1 {
			t.Errorf("root key %q assigned %d times, want exactly 1:\n%s", key, n, out)
		}
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
	if strings.Contains(out, "old-gateway.example") {
		t.Errorf("stale gateway base_url survived:\n%s", out)
	}
	if strings.Contains(out, `wire_api = "chat"`) {
		t.Errorf("stale wire_api survived; Codex only speaks responses:\n%s", out)
	}
	if !strings.Contains(out, `approval_policy = "never"`) {
		t.Errorf("unmanaged key was dropped:\n%s", out)
	}
}

func TestMergeCodexConfig_LeavesSameNamedKeysInOtherTables(t *testing.T) {
	// `model` under someone else's table is not ours to remove: only the
	// root-level assignment is managed.
	target := writeTemp(t, `approval_policy = "never"

[profiles.experiment]
model = "kept-by-employee"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !strings.Contains(string(got), `model = "kept-by-employee"`) {
		t.Errorf("key inside a non-managed table was removed:\n%s", got)
	}
}

func TestMergeCodexConfig_ManagedTableEndsAtNextHeader(t *testing.T) {
	target := writeTemp(t, `[model_providers.gateway]
base_url = "https://old/v1"

[model_providers.other]
base_url = "https://other/v1"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)
	if !strings.Contains(out, "[model_providers.other]") || !strings.Contains(out, "https://other/v1") {
		t.Errorf("stripping the managed table swallowed the following table:\n%s", out)
	}
	if strings.Contains(out, "https://old/v1") {
		t.Errorf("managed table body survived:\n%s", out)
	}
}

func TestMergeCodexConfig_EmptyExistingFile(t *testing.T) {
	target := writeTemp(t, "   \n\n")
	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if string(got) != deliveredConfig {
		t.Fatalf("empty file should yield the delivered config, got:\n%s", got)
	}
}

// TestMergeKeepsPreservedRootKeysAtRootLevel is the regression case for the
// bug this file exists to fix: a preserved root key must stay at the root
// of the document, never fall after the delivered [model_providers.gateway]
// header where TOML would silently reinterpret it as that table's field.
func TestMergeKeepsPreservedRootKeysAtRootLevel(t *testing.T) {
	target := writeTemp(t, `# my notes
approval_policy = "never"

[projects."C:\\work"]
trust_level = "trusted"

[mcp_servers.foo]
command = "foo.exe"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	firstHeader := strings.Index(out, "\n[")
	if firstHeader == -1 {
		t.Fatalf("expected at least one table header in output:\n%s", out)
	}
	rootSection := out[:firstHeader]

	for _, want := range []string{"# my notes", `approval_policy = "never"`} {
		if !strings.Contains(rootSection, want) {
			t.Errorf("preserved root content %q ended up after a table header:\n%s", want, out)
		}
	}

	// The comment must stay directly attached to the key it annotates, not
	// just present somewhere at the root.
	if !strings.Contains(out, "# my notes\napproval_policy = \"never\"") {
		t.Errorf("comment must stay attached to approval_policy:\n%s", out)
	}

	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
	if !strings.Contains(out, `base_url = "https://litellm.teenet.app/v1"`) {
		t.Errorf("delivered gateway settings missing:\n%s", out)
	}

	gatewayIdx := strings.Index(out, "[model_providers.gateway]")
	projectsIdx := strings.Index(out, `[projects."C:\\work"]`)
	mcpIdx := strings.Index(out, "[mcp_servers.foo]")
	if gatewayIdx == -1 || projectsIdx == -1 || mcpIdx == -1 {
		t.Fatalf("missing expected table(s):\n%s", out)
	}
	if !(gatewayIdx < projectsIdx && projectsIdx < mcpIdx) {
		t.Errorf("preserved tables must follow the delivered table, in original order: gateway=%d projects=%d mcp=%d\n%s", gatewayIdx, projectsIdx, mcpIdx, out)
	}
	if !strings.Contains(out, `trust_level = "trusted"`) {
		t.Errorf("preserved table content dropped:\n%s", out)
	}
	if !strings.Contains(out, `command = "foo.exe"`) {
		t.Errorf("preserved table content dropped:\n%s", out)
	}
}

func TestMergeWithNoPreservedRootKeys(t *testing.T) {
	tables := `[projects."C:\\work"]
trust_level = "trusted"

[mcp_servers.foo]
command = "foo.exe"
`
	target := writeTemp(t, tables)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	want := strings.TrimRight(deliveredConfig, "\n") + "\n\n" + tables
	if string(got) != want {
		t.Fatalf("expected delivered fragment then untouched tables, got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMergeWithNoPreservedTables(t *testing.T) {
	target := writeTemp(t, `# keep me
approval_policy = "never"
sandbox_mode    = "workspace-write"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if strings.Count(out, "[") != 1 {
		t.Fatalf("expected exactly one table header (the delivered one), got:\n%s", out)
	}
	if !strings.Contains(out, "[model_providers.gateway]") {
		t.Errorf("delivered gateway table missing:\n%s", out)
	}
	beforeTable := out[:strings.Index(out, "[")]
	for _, want := range []string{"# keep me", `approval_policy = "never"`, `sandbox_mode    = "workspace-write"`} {
		if !strings.Contains(beforeTable, want) {
			t.Errorf("preserved root content %q missing before the delivered table:\n%s", want, out)
		}
	}
	// Nothing should follow the delivered table: no preserved tables exist.
	afterTable := strings.TrimRight(out[strings.Index(out, "[model_providers.gateway]"):], "\n")
	if !strings.HasSuffix(afterTable, `wire_api = "responses"`) {
		t.Errorf("unexpected trailing content after the delivered table:\n%s", out)
	}
}

// TestMergeDoesNotStripManagedKeyNamesFromOtherProviderTables pins (or, if
// it were not already true, would force) the scoping of stripCodexManaged:
// key names shared with the managed set ("name", "base_url", "wire_api",
// "experimental_bearer_token", ...) belong to the employee when they sit in
// a different provider table, and must never be dropped just because they
// share a name with a managed key.
func TestMergeDoesNotStripManagedKeyNamesFromOtherProviderTables(t *testing.T) {
	target := writeTemp(t, `[model_providers.openai]
name     = "OpenAI"
base_url = "https://api.openai.com/v1"
wire_api = "responses"
experimental_bearer_token = "sk-employee-own-key"

[model_providers.gateway]
name     = "OldGateway"
base_url = "https://old-gateway.example/v1"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	for _, want := range []string{
		"[model_providers.openai]",
		`name     = "OpenAI"`,
		`base_url = "https://api.openai.com/v1"`,
		`wire_api = "responses"`,
		`experimental_bearer_token = "sk-employee-own-key"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("field %q in a non-managed provider table was dropped:\n%s", want, out)
		}
	}
	if strings.Contains(out, "OldGateway") || strings.Contains(out, "old-gateway.example") {
		t.Errorf("stale gateway table survived:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
}

// countRootAssignments counts `key = ...` lines that sit at the root of the
// document, ignoring occurrences inside table headers or other tables.
func countRootAssignments(doc, key string) int {
	n := 0
	inRoot := true
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inRoot = false
			continue
		}
		if !inRoot {
			continue
		}
		if got, ok := tomlRootKey(trimmed); ok && got == key {
			n++
		}
	}
	return n
}
