package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad json line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestOpsAndAuditGoToSeparateFilesWithCommonFields(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "console-test")
	if err != nil {
		t.Fatal(err)
	}
	w.Ops("warn", "platform_event", "oss slow", map[string]any{"latency_ms": 1200})
	w.Audit("admin_action", "quota changed", map[string]any{"employee_id": "alice", "action": "quota"})

	ops := readLines(t, filepath.Join(dir, "admin.jsonl"))
	aud := readLines(t, filepath.Join(dir, "audit.jsonl"))
	if len(ops) != 1 || len(aud) != 1 {
		t.Fatalf("want 1 ops + 1 audit line, got %d + %d", len(ops), len(aud))
	}
	for _, e := range []map[string]any{ops[0], aud[0]} {
		for _, k := range []string{"schema_version", "event_id", "event_type", "occurred_at", "module", "source_id", "level", "message"} {
			if _, ok := e[k]; !ok {
				t.Errorf("missing %s in %v", k, e)
			}
		}
		if e["module"] != "console" || e["source_id"] != "console-test" {
			t.Errorf("module/source wrong: %v", e)
		}
		if !strings.HasSuffix(e["occurred_at"].(string), "Z") {
			t.Errorf("occurred_at not UTC: %v", e["occurred_at"])
		}
	}
	if ops[0]["level"] != "warn" || ops[0]["latency_ms"].(float64) != 1200 {
		t.Errorf("ops fields: %v", ops[0])
	}
	if aud[0]["level"] != "info" || aud[0]["employee_id"] != "alice" {
		t.Errorf("audit fields: %v", aud[0])
	}
	if ops[0]["event_id"] == aud[0]["event_id"] {
		t.Errorf("event ids must differ")
	}
}

func TestEmptyDirWritesNothingAndDoesNotFail(t *testing.T) {
	w, err := New("", "x")
	if err != nil {
		t.Fatal(err)
	}
	w.Ops("info", "agent_event", "no dir", nil)
	w.Audit("admin_action", "no dir", nil)
}

func TestRotationRenamesAndKeepsBackups(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	w.ops.maxBytes = 400
	w.ops.backups = 2
	for i := 0; i < 20; i++ {
		w.Ops("info", "platform_event", strings.Repeat("x", 100), nil)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin.jsonl.1")); err != nil {
		t.Fatalf("expected admin.jsonl.1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin.jsonl.3")); err == nil {
		t.Fatalf("admin.jsonl.3 must not exist with backups=2")
	}
	// The live file was reopened, not truncated in place: it must be small
	// and end with a full line.
	b, _ := os.ReadFile(filepath.Join(dir, "admin.jsonl"))
	if len(b) == 0 || b[len(b)-1] != '\n' || len(b) > 400+300 {
		t.Fatalf("live file wrong after rotation: %d bytes", len(b))
	}
}

func TestFieldsAreRedactedBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir, "x")
	w.Ops("error", "platform_event", "gateway said no", map[string]any{
		"authorization": "Bearer sk-abcdef", "detail": "key sk-1234567890 rejected", "ok": true,
	})
	raw, _ := os.ReadFile(filepath.Join(dir, "admin.jsonl"))
	if strings.Contains(string(raw), "sk-abcdef") || strings.Contains(string(raw), "sk-1234567890") {
		t.Fatalf("secret leaked: %s", raw)
	}
}
