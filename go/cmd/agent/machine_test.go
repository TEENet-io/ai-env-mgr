package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNewLocalMachineHasAName(t *testing.T) {
	m, err := newLocalMachine()
	if err != nil {
		t.Fatalf("newLocalMachine: %v", err)
	}
	if m.Name() == "" {
		t.Error("machine name must not be empty")
	}
}

// The users root is faked so the scan can be exercised off Windows.
func fakeUsersRoot(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AIENVMGR_USERS_ROOT", root)
	return root
}

func TestLocalUsersSkipsSystemProfiles(t *testing.T) {
	fakeUsersRoot(t, "Administrator", "work1", "Public", "Default", "All Users", "systemprofile")

	m := &localMachine{name: "TEST"}
	got := m.LocalUsers()

	want := map[string]bool{"Administrator": true, "work1": true}
	if len(got) != len(want) {
		t.Fatalf("LocalUsers() = %v, want just the real accounts", got)
	}
	for _, u := range got {
		if !want[u] {
			t.Errorf("LocalUsers() included the system profile %q", u)
		}
	}
}

func TestLocalUsersIsSorted(t *testing.T) {
	fakeUsersRoot(t, "zoe", "adam", "mike")

	m := &localMachine{name: "TEST"}
	got := m.LocalUsers()

	if len(got) != 3 || got[0] != "adam" || got[2] != "zoe" {
		t.Errorf("LocalUsers() = %v, want sorted order", got)
	}
}

func TestLocalUsersSkipsFilesAndHiddenEntries(t *testing.T) {
	root := fakeUsersRoot(t, "work1", ".hidden")
	if err := os.WriteFile(filepath.Join(root, "desktop.ini"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &localMachine{name: "TEST"}
	got := m.LocalUsers()

	if len(got) != 1 || got[0] != "work1" {
		t.Errorf("LocalUsers() = %v, want only work1", got)
	}
}

func TestLocalUsersHandlesMissingRoot(t *testing.T) {
	t.Setenv("AIENVMGR_USERS_ROOT", filepath.Join(t.TempDir(), "does-not-exist"))

	m := &localMachine{name: "TEST"}
	if got := m.LocalUsers(); len(got) != 0 {
		t.Errorf("LocalUsers() = %v, want empty when the root is unreadable", got)
	}
}

func TestProfileDir(t *testing.T) {
	root := fakeUsersRoot(t)

	m := &localMachine{name: "TEST"}
	want := filepath.Join(root, "work1")
	if got := m.ProfileDir("work1"); got != want {
		t.Errorf("ProfileDir = %q, want %q", got, want)
	}
}

func TestStateDirIsAbsolute(t *testing.T) {
	if got := stateDir(); !filepath.IsAbs(got) {
		t.Errorf("stateDir() = %q, want an absolute path", got)
	}
}

func TestOrDash(t *testing.T) {
	if got := orDash(""); got != "-" {
		t.Errorf("orDash(\"\") = %q, want -", got)
	}
	if got := orDash("abc"); got != "abc" {
		t.Errorf("orDash(\"abc\") = %q, want abc", got)
	}
}

// Codex CLI creates its own sandbox accounts on Windows. They are not people
// and must not be reported as employee profiles, or every machine would flag
// them as unexpected accounts on every sync.
func TestLocalUsersSkipsToolingProfiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"work1", "work2",
		"CodexSandboxOffline", "CodexSandboxOnline",
		"Public", "Default", "systemprofile",
	} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AIENVMGR_USERS_ROOT", root)

	m := &localMachine{name: "TEST"}
	got := m.LocalUsers()

	want := []string{"work1", "work2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LocalUsers() = %v, want %v", got, want)
	}
}

func TestIsSystemProfile(t *testing.T) {
	cases := map[string]bool{
		"work1":                false,
		"jsmith":               false,
		"CodexSandboxOffline":  true,
		"CodexSandboxOnline":   true,
		"codexsandboxwhatever": true,
		"Public":               true,
		"systemprofile":        true,
		"Administrator":        false, // a real account; flagged elsewhere, not hidden
	}
	for name, want := range cases {
		if got := isSystemProfile(name); got != want {
			t.Errorf("isSystemProfile(%q) = %v, want %v", name, got, want)
		}
	}
}
