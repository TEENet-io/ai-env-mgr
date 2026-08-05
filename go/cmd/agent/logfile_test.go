package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sync interval can be set as low as one minute, so an append-only log
// would grow without bound on a machine nobody logs into.
func TestOpenLogRollsWhenLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	big := strings.Repeat("x", maxLogBytes+1)
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := openLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "fresh line")
	f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= int64(maxLogBytes) {
		t.Errorf("the live log should have been rolled, still %d bytes", info.Size())
	}

	// The previous generation must survive: it holds what happened before.
	old, err := os.Stat(path + logBackupSuffix)
	if err != nil {
		t.Fatalf("the previous log should be kept: %v", err)
	}
	if old.Size() != int64(len(big)) {
		t.Errorf("rolled log is %d bytes, want %d", old.Size(), len(big))
	}
}

// A log below the threshold must keep accumulating rather than being rolled
// on every start, which would throw away history at each reboot.
func TestOpenLogAppendsWhenSmall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := openLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "appended")
	f.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "existing") || !strings.Contains(string(data), "appended") {
		t.Errorf("both lines should be present, got %q", data)
	}
	if _, err := os.Stat(path + logBackupSuffix); err == nil {
		t.Error("a small log should not have been rolled")
	}
}

func TestOpenLogCreatesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	f, err := openLog(dir)
	if err != nil {
		t.Fatalf("openLog on an empty directory: %v", err)
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(dir, "agent.log")); err != nil {
		t.Errorf("the log should have been created: %v", err)
	}
}
