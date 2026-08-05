package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The credentials are plain strings in the source, so whichever binary is
// compiled from a package carries them verbatim. That is fine as long as the
// two sets stay in separate packages: agent.exe ships to employees' machines
// and must never contain the administrator's read-write key.
//
// This test compiles both binaries with distinctive keys injected through the
// linker and checks that each one carries only its own.
func TestAdminKeyNeverLandsInTheAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain available")
	}

	const (
		agentKey = "AGENT-KEY-SENTINEL-0e4c1f"
		adminKey = "ADMIN-KEY-SENTINEL-9b73da"
	)

	dir := t.TempDir()
	agentExe := buildWithKey(t, dir, "agent", agentKey)
	adminExe := buildWithKey(t, dir, "admin", adminKey)

	agentBin, err := os.ReadFile(agentExe)
	if err != nil {
		t.Fatal(err)
	}
	adminBin, err := os.ReadFile(adminExe)
	if err != nil {
		t.Fatal(err)
	}

	// Each binary must carry its own key -- otherwise the test is vacuous,
	// because a linker flag that silently failed would also produce two
	// binaries with no sentinel in either.
	if !bytes.Contains(agentBin, []byte(agentKey)) {
		t.Fatal("agent.exe does not carry its own key; the injection did not take, so this test proves nothing")
	}
	if !bytes.Contains(adminBin, []byte(adminKey)) {
		t.Fatal("admin.exe does not carry its own key; the injection did not take, so this test proves nothing")
	}

	// The point of the split.
	if bytes.Contains(agentBin, []byte(adminKey)) {
		t.Error("agent.exe contains the administrator's key -- the credentials must stay in separate packages")
	}
	if bytes.Contains(adminBin, []byte(agentKey)) {
		t.Error("admin.exe contains the agent's key")
	}
}

func buildWithKey(t *testing.T, dir, pkg, key string) string {
	t.Helper()
	out := filepath.Join(dir, pkg+".bin")
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-s -w -X main.ossAccessKeySecret="+key,
		"-o", out, "github.com/TEENet-io/airlock/cmd/"+pkg)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s failed: %v\n%s", pkg, err, msg)
	}
	return out
}
