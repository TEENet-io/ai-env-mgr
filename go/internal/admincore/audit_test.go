package admincore

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestAuditAppendsOneLinePerAction(t *testing.T) {
	m, store := newManager()
	m.appendAudit("alice", AuditOnboard, map[string]any{"budget": 20})
	m.appendAudit("alice", AuditQuota, map[string]any{"budget": 30})

	raw := string(store.objects[AuditKey("alice")])
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two JSONL lines, got %q", raw)
	}
	if !strings.Contains(lines[0], `"action":"onboard"`) || !strings.Contains(lines[1], `"action":"quota"`) {
		t.Errorf("lines out of order or mislabelled:\n%s", raw)
	}
}

func TestAuditReadReturnsNewestFirst(t *testing.T) {
	m, _ := newManager()
	m.appendAudit("alice", AuditOnboard, nil)
	m.appendAudit("alice", AuditOffboard, nil)
	entries, err := m.ReadAudit("alice")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 2 || entries[0].Action != AuditOffboard || entries[1].Action != AuditOnboard {
		t.Errorf("expected newest first, got %+v", entries)
	}
	if entries[0].User != "alice" || entries[0].At == "" {
		t.Errorf("entry missing user or timestamp: %+v", entries[0])
	}
}

func TestAuditReadCapsAtLimit(t *testing.T) {
	m, _ := newManager()
	for i := 0; i < auditReadLimit+5; i++ {
		m.appendAudit("alice", AuditQuota, map[string]any{"i": i})
	}
	entries, err := m.ReadAudit("alice")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != auditReadLimit {
		t.Errorf("got %d entries, want %d", len(entries), auditReadLimit)
	}
	if entries[0].Detail["i"] != float64(auditReadLimit+4) {
		t.Errorf("newest entry should be first, got %+v", entries[0].Detail)
	}
}

func TestAuditMissingFileIsEmpty(t *testing.T) {
	m, _ := newManager()
	entries, err := m.ReadAudit("nobody")
	if err != nil || len(entries) != 0 {
		t.Fatalf("no file should read as no history: %v %v", entries, err)
	}
}

func TestAuditWriteFailureDoesNotPanic(t *testing.T) {
	m, store := newManager()
	store.putErr = errNotFound
	m.appendAudit("alice", AuditOnboard, nil) // must only log
}

func TestAuditSkipsWriteWhenReadFailsTransiently(t *testing.T) {
	m, store := newManager()
	key := AuditKey("alice")
	seedLine := []byte(`{"at":"2026-01-01T00:00:00Z","action":"onboard","user":"alice"}`)
	store.objects[key] = seedLine

	// Make Get fail with a transient error for this key.
	store.getErrFor = key
	store.getErr = errors.New("transient network failure")

	m.appendAudit("alice", AuditQuota, map[string]any{"budget": 30})

	// The seeded line should be unchanged.
	if !bytes.Equal(store.objects[key], seedLine) {
		t.Errorf("history was modified on transient read failure: expected %q, got %q", seedLine, store.objects[key])
	}
}
