package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Agent releases are credential-free. The administrator may still carry its
// own runtime-configurable template, but no linker secret may land in the
// employee binary.
func TestAgentNeverCarriesAnOSSKey(t *testing.T) {
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

	if !bytes.Contains(adminBin, []byte(adminKey)) {
		t.Fatal("admin linker test did not take")
	}
	if bytes.Contains(agentBin, []byte(agentKey)) {
		t.Error("agent.exe contains an OSS key")
	}
	if bytes.Contains(agentBin, []byte(adminKey)) {
		t.Error("agent.exe contains the administrator's key -- the credentials must stay in separate packages")
	}
}

func buildWithKey(t *testing.T, dir, pkg, key string) string {
	t.Helper()
	out := filepath.Join(dir, pkg+".bin")
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-s -w -X main.ossAccessKeySecret="+key,
		"-o", out, "github.com/TEENet-io/ai-env-mgr/cmd/"+pkg)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s failed: %v\n%s", pkg, err, msg)
	}
	return out
}
