package codexinstructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func profile(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, ".codex", "AGENTS.md")
}

func TestAddsKeepsAndRemovesOnlyTheManagedBlock(t *testing.T) {
	dir, path := profile(t)
	own := "# 我的习惯\n\n回答用中文。\n"
	if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := Sync(dir, true); err != nil || !changed {
		t.Fatalf("add: %v %v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), own) || strings.Count(string(data), Begin) != 1 || !strings.Contains(string(data), "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8") {
		t.Fatalf("added:\n%s", data)
	}
	if changed, err := Sync(dir, true); err != nil || changed {
		t.Fatalf("second sync must be a no-op: %v %v", changed, err)
	}
	// An edited block is put back; the employee's text after it survives.
	edited := strings.Replace(string(data), "Get-Content", "cat", 1) + "\n更多我的内容\n"
	os.WriteFile(path, []byte(edited), 0o644)
	if changed, err := Sync(dir, true); err != nil || !changed {
		t.Fatalf("repair: %v %v", changed, err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "Get-Content -Raw -Encoding UTF8") || !strings.Contains(string(data), "更多我的内容") || !strings.HasPrefix(string(data), own) {
		t.Fatalf("repaired:\n%s", data)
	}
	if changed, err := Sync(dir, false); err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), Begin) || !strings.HasPrefix(string(data), own) || !strings.Contains(string(data), "更多我的内容") {
		t.Fatalf("removed:\n%s", data)
	}
}

func TestCreatesAndDeletesAFileItOwnsAlone(t *testing.T) {
	dir, path := profile(t)
	if changed, err := Sync(dir, true); err != nil || !changed {
		t.Fatalf("create: %v %v", changed, err)
	}
	if data, _ := os.ReadFile(path); !strings.HasPrefix(string(data), Begin) {
		t.Fatalf("created:\n%s", data)
	}
	if changed, err := Sync(dir, false); err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a file holding only the block is deleted")
	}
	if changed, err := Sync(dir, false); err != nil || changed {
		t.Fatalf("nothing to remove: %v %v", changed, err)
	}
}

func TestLeavesProfilesWithoutCodexAndRefusesDamagedBlocks(t *testing.T) {
	bare := t.TempDir()
	if changed, err := Sync(bare, true); err != nil || changed {
		t.Fatalf("no .codex: %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(bare, ".codex")); !os.IsNotExist(err) {
		t.Fatal(".codex must not be created")
	}
	for _, content := range []string{Begin + "\nhalf", "x\n" + End, Begin + "\n" + End + "\n" + Begin + "\n" + End} {
		dir, path := profile(t)
		os.WriteFile(path, []byte(content), 0o644)
		if _, err := Sync(dir, true); err == nil {
			t.Fatalf("damaged block accepted: %q", content)
		}
		if data, _ := os.ReadFile(path); string(data) != content {
			t.Fatal("a refused file must be left as it was")
		}
	}
}

func TestRefusesALinkedAgentsFile(t *testing.T) {
	dir, path := profile(t)
	target := filepath.Join(t.TempDir(), "elsewhere.md")
	os.WriteFile(target, []byte("x"), 0o644)
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := Sync(dir, true); err == nil {
		t.Fatal("linked AGENTS.md accepted")
	}
	if data, _ := os.ReadFile(target); string(data) != "x" {
		t.Fatal("link target was written")
	}
}
