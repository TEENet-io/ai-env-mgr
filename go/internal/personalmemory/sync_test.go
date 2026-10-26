package personalmemory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "memory_store.exe"), "MZhelper")
	write(filepath.Join(root, "seed", "personal-memory", "AGENTS-block.md"), startMarker+"\nRead personal cards.\n"+endMarker+"\n")
	for _, name := range skillNames {
		write(filepath.Join(root, "seed", "codex-home", "skills", name, "SKILL.md"), "# "+name+"\n")
	}
	return root
}

func TestInstallUpgradeAndRollbackPreserveUserFiles(t *testing.T) {
	root := testBundle(t)
	profile := t.TempDir()
	codexHome := filepath.Join(profile, ".codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	before := "# My own instructions\n\nKeep my workflow.\n"
	if err := os.WriteFile(filepath.Join(codexHome, "AGENTS.md"), []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := Sync(root, profile, "1.0", true)
	if err != nil || !changed {
		t.Fatalf("initial sync: changed=%v err=%v", changed, err)
	}
	changed, err = Sync(root, profile, "1.0", true)
	if err != nil || changed {
		t.Fatalf("unchanged sync should be a no-op: changed=%v err=%v", changed, err)
	}
	skill := filepath.Join(root, "seed", "codex-home", "skills", skillNames[0], "SKILL.md")
	if err := os.WriteFile(skill, []byte("# updated skill\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err = Sync(root, profile, "1.1", true)
	if err != nil || !changed {
		t.Fatalf("upgrade: changed=%v err=%v", changed, err)
	}
	installedSkill := filepath.Join(codexHome, "skills", skillNames[0], "SKILL.md")
	if data, err := os.ReadFile(installedSkill); err != nil || string(data) != "# updated skill\n" {
		t.Fatalf("updated skill: %q %v", data, err)
	}
	changed, err = Sync(root, profile, "", false)
	if err != nil || !changed {
		t.Fatalf("rollback: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(installedSkill); !os.IsNotExist(err) {
		t.Fatalf("managed skill survived rollback: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(codexHome, "AGENTS.md")); err != nil || !strings.HasPrefix(string(data), before) || strings.Contains(string(data), startMarker) {
		t.Fatalf("user AGENTS.md was damaged: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(profile, ".ai-memory")); err == nil {
		t.Fatal("sync should never create or delete personal card storage")
	}
}

func TestRefuseEditedSkillAndSymlinkEscape(t *testing.T) {
	root := testBundle(t)
	profile := t.TempDir()
	if _, err := Sync(root, profile, "1.0", true); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(profile, ".codex", "skills", skillNames[0], "SKILL.md")
	if err := os.WriteFile(target, []byte("my edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(root, profile, "1.1", true); err == nil || !strings.Contains(err.Error(), "edited") {
		t.Fatalf("edited managed skill should cause a conflict: %v", err)
	}
	if _, err := Sync(root, profile, "", false); err == nil || !strings.Contains(err.Error(), "edited") {
		t.Fatalf("rollback should preserve an edited skill: %v", err)
	}

	other := t.TempDir()
	linkedProfile := filepath.Join(t.TempDir(), "profile")
	if err := os.Symlink(other, linkedProfile); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Sync(root, linkedProfile, "1.0", true); err == nil {
		t.Fatal("symlinked employee profile was accepted")
	}
	if _, err := os.Stat(filepath.Join(other, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("sync wrote outside employee profile: %v", err)
	}
}
