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

// --- second-round regressions found in review ---

// TestSplitDoesNotSeverMultiLineArray reproduces the review's first finding:
// a naive per-line "starts with [ and ends with ]" header check misreads a
// line inside a multi-line array, such as the `[1, 2]` element below, as a
// table header and injects the delivered gateway table in the middle of it.
func TestSplitDoesNotSeverMultiLineArray(t *testing.T) {
	block := `multi = [
  "a",
  [1, 2]
]
`
	target := writeTemp(t, block)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	// A contiguous, unbroken match of the whole block is the point: if the
	// delivered table had been injected in the middle of the array (between
	// "a", and [1, 2], as the bug reproduced), this substring would not be
	// found at all.
	if !strings.Contains(out, block) {
		t.Errorf("multi-line array was severed by the merge:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
}

// TestSplitDoesNotSeverMultiLineString reproduces the review's second
// finding: a line inside a triple-quoted string that happens to look like a
// header (`[not a table]`) must not be treated as one -- doing so let the
// delivered gateway table (including its bearer token) get swallowed into
// the string instead of being written as an actual table.
func TestSplitDoesNotSeverMultiLineString(t *testing.T) {
	block := `notes = """
line one
[not a table]
line two
"""
`
	target := writeTemp(t, block)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if !strings.Contains(out, block) {
		t.Errorf("multi-line string was severed or swallowed the delivered content:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once (not swallowed into the string):\n%s", out)
	}
	if !strings.Contains(out, `base_url = "https://litellm.teenet.app/v1"`) {
		t.Errorf("delivered gateway settings missing, likely swallowed into the string:\n%s", out)
	}
}

// TestSplitStillHandlesArrayOfTablesHeader confirms the TOML-aware scanner
// still recognizes a genuine [[array-of-tables]] header as a header.
func TestSplitStillHandlesArrayOfTablesHeader(t *testing.T) {
	target := writeTemp(t, `[[array_of_tables]]
name = "one"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if !strings.Contains(out, "[[array_of_tables]]") || !strings.Contains(out, `name = "one"`) {
		t.Errorf("array-of-tables header/content missing:\n%s", out)
	}
	gatewayIdx := strings.Index(out, "[model_providers.gateway]")
	arrayIdx := strings.Index(out, "[[array_of_tables]]")
	if gatewayIdx == -1 || arrayIdx == -1 || gatewayIdx > arrayIdx {
		t.Errorf("delivered table must precede the preserved array-of-tables:\n%s", out)
	}
}

// TestSplitIgnoresHeaderLookingCommentLine confirms a comment that merely
// contains bracket text (`#[not a header]`) is never mistaken for a header.
func TestSplitIgnoresHeaderLookingCommentLine(t *testing.T) {
	target := writeTemp(t, `#[not a header]
approval_policy = "never"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	gatewayIdx := strings.Index(out, "[model_providers.gateway]")
	commentIdx := strings.Index(out, "#[not a header]")
	apIdx := strings.Index(out, `approval_policy = "never"`)
	if gatewayIdx == -1 || commentIdx == -1 || apIdx == -1 {
		t.Fatalf("missing expected content:\n%s", out)
	}
	if commentIdx > gatewayIdx || apIdx > gatewayIdx {
		t.Errorf("comment/root key that only look like a header must stay at root, before the delivered table:\n%s", out)
	}
}

// TestStripDoesNotLeakManagedTableThroughNestedArray reproduces the review's
// "minor": a nested array inside the managed gateway table's own fields used
// to be misread as the start of the next table, ending the "still stripping
// the managed table" state early and letting the rest of the table -- the
// stale base_url and bearer token included -- leak through as if it were
// preserved content.
func TestStripDoesNotLeakManagedTableThroughNestedArray(t *testing.T) {
	target := writeTemp(t, `[model_providers.gateway]
base_url = "https://old-gateway.example/v1"
extra = [
  "x",
  [1, 2]
]
experimental_bearer_token = "sk-OLD-LEAKED"

[model_providers.other]
base_url = "https://other/v1"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if strings.Contains(out, "sk-OLD-LEAKED") {
		t.Errorf("stale bearer token leaked past the strip via a nested array:\n%s", out)
	}
	if strings.Contains(out, "old-gateway.example") {
		t.Errorf("stale gateway base_url leaked past the strip via a nested array:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
	if !strings.Contains(out, "[model_providers.other]") || !strings.Contains(out, "https://other/v1") {
		t.Errorf("the following table must survive the strip intact:\n%s", out)
	}
}

// TestSplitAttachesImmediatePrecedingCommentToTable reproduces the review's
// third finding: a comment documenting the table right below it must move
// with that table, not stay behind in the root section above the delivered
// gateway table.
func TestSplitAttachesImmediatePrecedingCommentToTable(t *testing.T) {
	target := writeTemp(t, `approval_policy = "never"

# notes about foo
[mcp_servers.foo]
command = "foo.exe"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if !strings.Contains(out, "# notes about foo\n[mcp_servers.foo]") {
		t.Errorf("comment must stay directly attached to its table:\n%s", out)
	}
	gatewayIdx := strings.Index(out, "[model_providers.gateway]")
	commentIdx := strings.Index(out, "# notes about foo")
	apIdx := strings.Index(out, `approval_policy = "never"`)
	if gatewayIdx == -1 || commentIdx == -1 || apIdx == -1 {
		t.Fatalf("missing expected content:\n%s", out)
	}
	if apIdx > gatewayIdx {
		t.Errorf("preserved root key must stay before the delivered table:\n%s", out)
	}
	if commentIdx < gatewayIdx {
		t.Errorf("comment must move with its table, after the delivered table, not stay at root:\n%s", out)
	}
}

// TestSplitAttachesTrailingCommentAndBlankRunToTable covers the root section
// ending in a trailing comment followed by a blank line and then the header:
// both the comment and the blank line must move with the table.
func TestSplitAttachesTrailingCommentAndBlankRunToTable(t *testing.T) {
	target := writeTemp(t, `approval_policy = "never"
# trailing comment

[mcp_servers.foo]
command = "foo.exe"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	gatewayIdx := strings.Index(out, "[model_providers.gateway]")
	commentIdx := strings.Index(out, "# trailing comment")
	mcpIdx := strings.Index(out, "[mcp_servers.foo]")
	apIdx := strings.Index(out, `approval_policy = "never"`)
	if gatewayIdx == -1 || commentIdx == -1 || mcpIdx == -1 || apIdx == -1 {
		t.Fatalf("missing expected content:\n%s", out)
	}
	if apIdx > gatewayIdx {
		t.Errorf("preserved root key must stay before the delivered table:\n%s", out)
	}
	if !(gatewayIdx < commentIdx && commentIdx < mcpIdx) {
		t.Errorf("trailing comment (and its blank line) must move with the table it precedes:\n%s", out)
	}
}

// TestMergeIsIdempotent is the regression case for the review's third
// finding: merging an already-merged file with the same delivered fragment
// again must produce byte-identical output, or deploy.go's
// bytes.Equal(previous, payload) check never holds and every delivery
// rewrites config.toml -- and grows it by one blank line -- forever.
func TestMergeIsIdempotent(t *testing.T) {
	target := writeTemp(t, `# leading comment
approval_policy = "never"
multi = [
  "a",
  "b",
]

# table comment
[mcp_servers.foo]
command = "foo.exe"
`)

	first, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}

	target2 := writeTemp(t, string(first))
	second, err := mergeCodexConfig(target2, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}

	if string(first) != string(second) {
		t.Fatalf("merge is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// --- round 3: scoped re-review findings ---

// TestStripKeepsCommentImmediatelyAboveGatewayHeaderAtRoot is F1(a): a
// comment with nothing following it in the existing file ends up, after
// merge 1's own assembly, sitting directly above the delivered gateway
// header (there being no other table for it to be ordered before). Merge 2
// must not then delete it as if it were inside the managed table it merely
// happens to precede.
func TestStripKeepsCommentImmediatelyAboveGatewayHeaderAtRoot(t *testing.T) {
	target := writeTemp(t, `approval_policy = "never"
# my personal note
`)

	first, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if !strings.Contains(string(first), "# my personal note") {
		t.Fatalf("comment lost on first merge:\n%s", first)
	}

	target2 := writeTemp(t, string(first))
	second, err := mergeCodexConfig(target2, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if !strings.Contains(string(second), "# my personal note") {
		t.Errorf("comment lost on second merge:\n%s", second)
	}
	if string(first) != string(second) {
		t.Fatalf("merge is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestStripKeepsCommentAboveExistingGatewayTable is F1(b): a comment already
// sitting above the employee's OWN existing gateway table (which is about
// to be wholesale-replaced) must survive the very first merge, not just a
// second one.
func TestStripKeepsCommentAboveExistingGatewayTable(t *testing.T) {
	target := writeTemp(t, `approval_policy = "never"

# unrelated note about proxies

[model_providers.gateway]
base_url = "https://old/v1"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if !strings.Contains(out, "# unrelated note about proxies") {
		t.Errorf("comment above the (replaced) gateway table was deleted:\n%s", out)
	}
	if strings.Contains(out, "https://old/v1") {
		t.Errorf("stale gateway base_url survived:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
}

// TestSplitDoesNotSeverMultiLineArrayContainingBlankLine and
// TestSplitDoesNotSeverMultiLineStringContainingBlankLine are the safety net
// for F2/F3's recovery pass: a blank line inside a genuine, properly closed
// multi-line array or string must never trigger recovery -- recovery only
// ever runs at all when the file, scanned once straight through, ends with
// an array still open, which a properly closed one never does.
func TestSplitDoesNotSeverMultiLineArrayContainingBlankLine(t *testing.T) {
	block := `multi = [
  "a",

  "b"
]
`
	target := writeTemp(t, block)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if !strings.Contains(out, block) {
		t.Errorf("multi-line array with an interior blank line was severed:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
}

func TestSplitDoesNotSeverMultiLineStringContainingBlankLine(t *testing.T) {
	block := `notes = """
line one

line two
"""
`
	target := writeTemp(t, block)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if !strings.Contains(out, block) {
		t.Errorf("multi-line string with an interior blank line was severed:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
}

// TestMergeTwiceRecoversFromUnterminatedArrayWithoutDuplicatingGateway is
// F2: an unterminated `[` makes depth stick for the rest of the file, so on
// the first merge the real header inside it goes unrecognized (harmless: it
// just ends up as leftover root text, "best effort" per the review). But
// the SECOND merge re-scans that same still-unterminated fragment, which
// this time also hides the real, now-present `[model_providers.gateway]`
// header from being stripped -- without recovery, the delivered table would
// be appended on top of the surviving old one, duplicating it, and every
// further merge would add one more copy.
func TestMergeTwiceRecoversFromUnterminatedArrayWithoutDuplicatingGateway(t *testing.T) {
	target := writeTemp(t, "a = [\n1,\n[mcp_servers.foo]\ncommand = \"keepme3\"\n")

	first, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}

	target2 := writeTemp(t, string(first))
	second, err := mergeCodexConfig(target2, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	out := string(second)

	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Fatalf("gateway table must appear exactly once after merging twice, got:\n%s", out)
	}
}

// TestMergeRecoversTableAfterUnterminatedManagedRootValue is F3: a managed
// root key (`model`) opening a value that never closes used to delete
// everything after it -- dropRootContinuation, once set when the managed
// key's opening line was dropped, was only ever cleared by a recognized
// header, and the unterminated array hid every later header from being
// recognized at all. The recovery pass added for F2 fixes this the same
// way: once a later line is recovered as a real header, the drop is over.
func TestMergeRecoversTableAfterUnterminatedManagedRootValue(t *testing.T) {
	target := writeTemp(t, "model = [\napproval_policy = \"never\"\n[mcp_servers.foo]\ncommand = \"keepme4\"\n")

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if !strings.Contains(out, "[mcp_servers.foo]") {
		t.Errorf("table after the unterminated managed value was lost:\n%s", out)
	}
	if !strings.Contains(out, `command = "keepme4"`) {
		t.Errorf("content of the table after the unterminated managed value was lost:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
	// approval_policy is inside the unterminated `model = [` value and may
	// legitimately be lost as part of it (this input is malformed); the
	// requirement is only that losing it does not take the rest of the
	// file down too, and that nothing panics.
}

// TestHeaderMatchTakesBracketInsideQuotedKeySegment is F4: a table name
// segment can be a quoted string, and a quoted string is allowed to contain
// a literal `]` -- `[servers."a]b"]` is a real header whose quoted key
// happens to contain one. A header matcher that stops at the first `]`
// regardless of quoting misses it, and everything from there on reads as
// leftover content of whatever table came before (here, the very table
// being wholesale-replaced), losing both the header and its body.
func TestHeaderMatchTakesBracketInsideQuotedKeySegment(t *testing.T) {
	target := writeTemp(t, `[model_providers.gateway]
base_url = "old"

[servers."a]b"]
command = "keepme"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	if !strings.Contains(out, `[servers."a]b"]`) {
		t.Errorf("header with a bracket inside a quoted key segment was lost:\n%s", out)
	}
	if !strings.Contains(out, `command = "keepme"`) {
		t.Errorf("content of the table with a bracketed quoted key was lost:\n%s", out)
	}
	if strings.Contains(out, `base_url = "old"`) {
		t.Errorf("stale gateway base_url survived:\n%s", out)
	}
	if strings.Count(out, "[model_providers.gateway]") != 1 {
		t.Errorf("gateway table must appear exactly once:\n%s", out)
	}
}

// TestHeaderMatchStillHandlesQuotedKeysWithoutBrackets pins the two quoted-
// key forms the review called out as already working: a quoted string
// without a `]` inside it, and a literal (single-quoted) string.
func TestHeaderMatchStillHandlesQuotedKeysWithoutBrackets(t *testing.T) {
	target := writeTemp(t, `[mcp_servers."my server"]
command = "a"

[mcp_servers.'lit']
command = "b"
`)

	got, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	out := string(got)

	for _, want := range []string{
		`[mcp_servers."my server"]`,
		`command = "a"`,
		`[mcp_servers.'lit']`,
		`command = "b"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("quoted-key table content missing %q:\n%s", want, out)
		}
	}
}

// TestMergeIsIdempotentUnderCommentsBlankRunsAndInteriorArrayBlank is F5: a
// stress case combining several formatting quirks in one preserved root --
// a trailing comment with no following table, blank runs longer than one
// line, a multi-line array with a blank line inside it, and (as a
// consequence of there being no table at all in the input) a comment
// directly above the delivered gateway header after merge 1. Merging twice
// must still be byte-identical.
func TestMergeIsIdempotentUnderCommentsBlankRunsAndInteriorArrayBlank(t *testing.T) {
	target := writeTemp(t, `# leading comment
approval_policy = "never"


multi = [
  "a",

  "b",
]

# trailing comment with no following table
`)

	first, err := mergeCodexConfig(target, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if !strings.Contains(string(first), "# trailing comment with no following table") {
		t.Fatalf("trailing comment lost on first merge:\n%s", first)
	}

	target2 := writeTemp(t, string(first))
	second, err := mergeCodexConfig(target2, []byte(deliveredConfig))
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}

	if string(first) != string(second) {
		t.Fatalf("merge is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(string(second), "# trailing comment with no following table") {
		t.Errorf("trailing comment above the gateway header was lost on re-merge:\n%s", second)
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

// --- round 4: unterminated triple-quoted strings, fragment comments ---

// tripleQuoteStyles are the two multi-line string delimiters TOML defines.
// Every unterminated-string case below must behave identically for both:
// the scanner tracks them with separate state flags, so a fix applied to
// only one of them is a fix for neither.
var tripleQuoteStyles = []struct {
	name  string
	delim string
}{
	{"basic", `"""`},
	{"literal", `'''`},
}

// mergeNTimes merges fragment into body n times, feeding each merge's output
// to the next, and returns every intermediate result.
func mergeNTimes(t *testing.T, body, fragment string, n int) []string {
	t.Helper()
	outs := make([]string, 0, n)
	current := body
	for i := 0; i < n; i++ {
		target := writeTemp(t, current)
		got, err := mergeCodexConfig(target, []byte(fragment))
		if err != nil {
			t.Fatalf("merge %d: %v", i+1, err)
		}
		current = string(got)
		outs = append(outs, current)
	}
	return outs
}

// TestMergeRepeatedlyWithUnterminatedTripleQuotedString is round 4's finding
// A(1): the round-3 recovery pass was triggered on leftover bracket depth
// alone, so a value opened with `"""` (or triple apostrophes) and never
// closed was never recovered from. Every later header -- including the
// `[model_providers.gateway]` header a previous merge itself wrote -- stayed
// hidden inside that string, so stripCodexManaged never removed the old
// table before the new one was appended: one more gateway table, and one
// more stale experimental_bearer_token, per delivery, forever.
func TestMergeRepeatedlyWithUnterminatedTripleQuotedString(t *testing.T) {
	for _, style := range tripleQuoteStyles {
		t.Run(style.name, func(t *testing.T) {
			body := "notes = " + style.delim + "\nunterminated\n"

			outs := mergeNTimes(t, body, deliveredConfig, 3)
			for i, out := range outs {
				if n := strings.Count(out, "[model_providers.gateway]"); n != 1 {
					t.Fatalf("merge %d produced %d gateway headers, want exactly 1:\n%s", i+1, n, out)
				}
			}
			if outs[0] != outs[1] {
				t.Errorf("merge 2 changed the file again:\n--- 1 ---\n%s\n--- 2 ---\n%s", outs[0], outs[1])
			}
			if outs[1] != outs[2] {
				t.Fatalf("merge is not idempotent:\n--- 2 ---\n%s\n--- 3 ---\n%s", outs[1], outs[2])
			}
			if !strings.Contains(outs[2], "notes = "+style.delim) {
				t.Errorf("the employee's (malformed) value was dropped entirely:\n%s", outs[2])
			}
		})
	}
}

// TestMergeRecoversTableAfterUnterminatedTripleQuotedManagedRootValue is
// round 4's finding A(2): a *managed* root key whose value opens a
// triple-quoted string that never closes used to take the whole rest of the
// file down with it -- stripCodexManaged's dropRootContinuation is cleared
// only by a recognized header, and no header downstream was recognized. The
// key's own value is malformed and may be lost; unrelated tables after it
// must not be.
func TestMergeRecoversTableAfterUnterminatedTripleQuotedManagedRootValue(t *testing.T) {
	for _, style := range tripleQuoteStyles {
		t.Run(style.name, func(t *testing.T) {
			body := "model = " + style.delim + "\napproval_policy = \"never\"\n[mcp_servers.foo]\ncommand = \"keepme\"\n"

			outs := mergeNTimes(t, body, deliveredConfig, 3)
			for i, out := range outs {
				if n := strings.Count(out, "[model_providers.gateway]"); n != 1 {
					t.Fatalf("merge %d produced %d gateway headers, want exactly 1:\n%s", i+1, n, out)
				}
				if !strings.Contains(out, "[mcp_servers.foo]") {
					t.Fatalf("merge %d lost the table after the unterminated value:\n%s", i+1, out)
				}
				if !strings.Contains(out, `command = "keepme"`) {
					t.Fatalf("merge %d lost the body of the table after the unterminated value:\n%s", i+1, out)
				}
			}
			if outs[1] != outs[2] {
				t.Fatalf("merge is not idempotent:\n--- 2 ---\n%s\n--- 3 ---\n%s", outs[1], outs[2])
			}
			// approval_policy sits inside the unterminated value and may
			// legitimately go with it; the requirement is only that its
			// loss does not extend to the rest of the file.
		})
	}
}

// TestValidTripleQuotedStringWithHeaderLookingLineIsNotRecovered is the
// safety net for the widened recovery trigger: a *valid* file whose
// multi-line string legitimately contains a header-shaped line still scans
// clean to EOF, so recovery never runs and `[not a table]` stays what it is
// -- string content, not a header.
func TestValidTripleQuotedStringWithHeaderLookingLineIsNotRecovered(t *testing.T) {
	for _, style := range tripleQuoteStyles {
		t.Run(style.name, func(t *testing.T) {
			block := "notes = " + style.delim + "\n[not a table]\n" + style.delim + "\n[mcp_servers.foo]\ncommand = \"x\"\n"

			lines, err := scanTomlLines([]byte(block))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			for _, ln := range lines {
				if strings.TrimSpace(ln.text) == "[not a table]" && ln.isHeader {
					t.Fatalf("a header-shaped line inside a closed multi-line string was classified as a header:\n%s", block)
				}
			}
			if !lines[3].isHeader || lines[3].header != "mcp_servers.foo" {
				t.Fatalf("the real header after the string was not classified as one: %+v", lines[3])
			}

			target := writeTemp(t, block)
			got, err := mergeCodexConfig(target, []byte(deliveredConfig))
			if err != nil {
				t.Fatalf("merge: %v", err)
			}
			out := string(got)
			// Contiguity is the point: had `[not a table]` been promoted,
			// the string would have been split around it.
			if !strings.Contains(out, "notes = "+style.delim+"\n[not a table]\n"+style.delim) {
				t.Errorf("the multi-line string was severed around its header-shaped line:\n%s", out)
			}
			if n := strings.Count(out, "[model_providers.gateway]"); n != 1 {
				t.Errorf("gateway table must appear exactly once, got %d:\n%s", n, out)
			}
			if !strings.Contains(out, "[mcp_servers.foo]") || !strings.Contains(out, `command = "x"`) {
				t.Errorf("the real table after the string was lost:\n%s", out)
			}
		})
	}
}

// TestFragmentCommentAboveManagedHeaderDoesNotAccumulate is round 4's
// finding B: a comment the *delivered fragment* writes directly above
// `[model_providers.gateway]` documents the managed table, so it must be
// stripped and re-delivered with that table like the table's own body. If
// it were instead treated as preserved employee content (as the blanket
// round-3 exemption did), every delivery would leave one more copy behind.
// The renderer emits no comments today, so this is a pin, not a bug fix.
func TestFragmentCommentAboveManagedHeaderDoesNotAccumulate(t *testing.T) {
	fragment := strings.Replace(deliveredConfig,
		"[model_providers.gateway]",
		"# Managed by IT -- do not edit\n[model_providers.gateway]", 1)

	body := `approval_policy = "never"
`
	outs := mergeNTimes(t, body, fragment, 3)
	for i, out := range outs {
		if n := strings.Count(out, "# Managed by IT -- do not edit"); n != 1 {
			t.Fatalf("merge %d left %d copies of the fragment's comment, want exactly 1:\n%s", i+1, n, out)
		}
		if !strings.Contains(out, "# Managed by IT -- do not edit\n[model_providers.gateway]") {
			t.Errorf("merge %d detached the fragment's comment from its table:\n%s", i+1, out)
		}
		if !strings.Contains(out, `approval_policy = "never"`) {
			t.Errorf("merge %d dropped preserved employee content:\n%s", i+1, out)
		}
	}
	if outs[0] != outs[1] || outs[1] != outs[2] {
		t.Fatalf("merge is not idempotent:\n--- 1 ---\n%s\n--- 2 ---\n%s\n--- 3 ---\n%s", outs[0], outs[1], outs[2])
	}
}
