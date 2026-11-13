//go:build windows

package managedskills

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"golang.org/x/sys/windows"
)

func TestWindowsCompanySkillFilesInheritReadOnlyACL(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires an elevated Windows test runner")
	}
	m := manager(t)
	m.Protect = Protect
	m.ProtectState = ProtectPrivate
	plan := model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle(1, "first")}}
	if _, err := m.Sync(plan); err != nil {
		t.Fatal(err)
	}
	root, err := windows.GetNamedSecurityInfo(m.Root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := root.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("root inherited writable permissions: %v", err)
	}
	check := func(path string, wantRead bool) {
		t.Helper()
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		read := false
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, i, &ace); err != nil {
				t.Fatal(err)
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
				continue
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.IsWellKnown(windows.WinBuiltinUsersSid) {
				continue
			}
			if uint32(ace.Mask)&(0x156|0x10000|0x40000|0x80000|0x40000000|0x10000000) != 0 {
				t.Fatalf("Users can write %s: %x", path, ace.Mask)
			}
			read = read || uint32(ace.Mask)&(0x80000000|1) != 0
		}
		if read != wantRead {
			t.Fatalf("Users read access to %s: got %v want %v", path, read, wantRead)
		}
	}
	check(filepath.Join(m.Root, "skills", "bippo-demo", "SKILL.md"), true)
	check(filepath.Join(m.StateDir, "company-skills", "state.json"), false)
	// A second sync must keep file readability and perform no cache refresh.
	if _, err := m.Sync(plan); err != nil {
		t.Fatal(err)
	}
	plan.Skills[0] = bundle(2, "second")
	if _, err := m.Sync(plan); err != nil {
		t.Fatal(err)
	}
	check(filepath.Join(m.Root, "skills", "bippo-demo", "SKILL.md"), true)
}

func TestWindowsRefusesPreexistingChildWithWritableExplicitACL(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevation")
	}
	root := filepath.Join(t.TempDir(), "Codex")
	child := filepath.Join(root, "skills")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(child, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := Protect(root); err == nil {
		t.Fatal("preexisting writable child was trusted")
	}
}
