package managedskills

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/companyskills"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func bundle(version int, body string) model.ManagedSkill {
	s := model.ManagedSkill{Name: "bippo-demo", Version: version, Files: map[string][]byte{"SKILL.md": []byte("---\nname: bippo-demo\ndescription: demo\n---\n" + body)}}
	s.SHA256 = companyskills.Digest(s.Files)
	return s
}

func manager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{Root: filepath.Join(t.TempDir(), "Codex"), StateDir: t.TempDir(), Protect: func(string) error { return nil }, ProtectState: func(string) error { return nil }, Stop: func(string) error { return nil }}
}

func TestInstallUpdateRollbackAndRevoke(t *testing.T) {
	m := manager(t)
	stops := 0
	m.Stop = func(user string) error {
		if user != "work1" {
			t.Fatalf("wrong user %s", user)
		}
		stops++
		return nil
	}
	plan := model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle(1, "first")}}
	st, err := m.Sync(plan)
	if err != nil || st.State != "applied" || stops != 0 {
		t.Fatalf("install: %+v %v stops=%d", st, err, stops)
	}
	if _, err := m.Sync(plan); err != nil || stops != 0 {
		t.Fatalf("unchanged: %v stops=%d", err, stops)
	}
	plan.Skills[0] = bundle(2, "second")
	if _, err := m.Sync(plan); err != nil || stops != 1 {
		t.Fatalf("update: %v stops=%d", err, stops)
	}
	plan.Skills[0] = bundle(1, "first")
	if _, err := m.Sync(plan); err != nil || stops != 2 {
		t.Fatalf("rollback: %v stops=%d", err, stops)
	}
	if _, err := m.Sync(model.ManagedSkills{}); err != nil || stops != 3 {
		t.Fatalf("revoke: %v stops=%d", err, stops)
	}
	if _, err := os.Stat(filepath.Join(m.Root, "skills", "bippo-demo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skill still present: %v", err)
	}
}

func TestRestartFailureSurvivesAgentRestartAndRetries(t *testing.T) {
	m := manager(t)
	if _, err := m.Sync(model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle(1, "first")}}); err != nil {
		t.Fatal(err)
	}
	m.Stop = func(string) error { return errors.New("kill failed") }
	st, err := m.Sync(model.ManagedSkills{})
	if err == nil || st.State != "restart_pending" {
		t.Fatalf("lost failure: %+v %v", st, err)
	}
	copy := *m
	stops := 0
	copy.Stop = func(user string) error {
		if user != "work1" {
			t.Fatal(user)
		}
		stops++
		return nil
	}
	if st, err = copy.Sync(model.ManagedSkills{}); err != nil || stops != 1 || st.State != "applied" {
		t.Fatalf("retry: %+v %v stops=%d", st, err, stops)
	}
}

func TestInterruptedSwapCleansBackupAndKeepsPendingRefresh(t *testing.T) {
	m := manager(t)
	plan := model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle(2, "current")}}
	if _, err := m.Sync(plan); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(m.Root, ".company-skills-staging", "bippo-demo.old")
	if err := os.MkdirAll(backup, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "SKILL.md"), []byte("old instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	j, err := m.load()
	if err != nil {
		t.Fatal(err)
	}
	j.Pending["work1"] = true
	if err := m.save(j); err != nil {
		t.Fatal(err)
	}
	stops := 0
	m.Stop = func(string) error { stops++; return nil }
	if _, err := m.Sync(plan); err != nil || stops != 1 {
		t.Fatalf("recovery: %v, stops=%d", err, stops)
	}
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale backup survived")
	}
}

func TestRebindingRefreshesOnlyPreviousAndNewEmployee(t *testing.T) {
	m := manager(t)
	plan := model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle(1, "same")}}
	if _, err := m.Sync(plan); err != nil {
		t.Fatal(err)
	}
	var users []string
	m.Stop = func(user string) error { users = append(users, user); return nil }
	plan.User = "work2"
	if _, err := m.Sync(plan); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0] != "work1" || users[1] != "work2" {
		t.Fatalf("wrong sessions: %v", users)
	}
}

func TestProtectAndOwnershipFailuresLeaveFilesAlone(t *testing.T) {
	m := manager(t)
	path := filepath.Join(m.Root, "skills", "bippo-demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unmanaged"), 0644); err != nil {
		t.Fatal(err)
	}
	plan := model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle(1, "first")}}
	if _, err := m.Sync(plan); err == nil {
		t.Fatal("overwrote unmanaged skill")
	}
	if data, _ := os.ReadFile(path); string(data) != "unmanaged" {
		t.Fatal("unmanaged file changed")
	}
	m = manager(t)
	m.Protect = func(string) error { return errors.New("ACL failed") }
	if _, err := m.Sync(plan); err == nil {
		t.Fatal("published without ACL")
	}
	if _, err := os.Stat(filepath.Join(m.Root, "skills", "bippo-demo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("skill written before protection")
	}
}

func TestRejectsSymlinksAndInvalidBundleBeforeRevocation(t *testing.T) {
	m := manager(t)
	plan := model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle(1, "first")}}
	if _, err := m.Sync(plan); err != nil {
		t.Fatal(err)
	}
	bad := bundle(2, "bad")
	bad.Files["../escape.md"] = []byte("escape")
	bad.SHA256 = companyskills.Digest(bad.Files)
	if _, err := m.Sync(model.ManagedSkills{User: "work2", Skills: []model.ManagedSkill{bad}}); err == nil {
		t.Fatal("bad plan accepted")
	}
	if _, err := os.Stat(filepath.Join(m.Root, "skills", "bippo-demo", "SKILL.md")); err != nil {
		t.Fatal("valid prior skill lost")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(m.Root, "skills", "bippo-link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	linked := bundle(1, "link")
	linked.Name = "bippo-link"
	linked.Files["SKILL.md"] = []byte("---\nname: bippo-link\ndescription: link\n---\nsteps")
	linked.SHA256 = companyskills.Digest(linked.Files)
	if _, err := m.Sync(model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{linked}}); err == nil {
		t.Fatal("symlink accepted")
	}
}
