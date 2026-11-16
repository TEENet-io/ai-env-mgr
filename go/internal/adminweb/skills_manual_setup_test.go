package adminweb

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestSkillsManualModelPackageIsAdminOnly(t *testing.T) {
	s, _ := newDatabaseServer(t)
	dir := t.TempDir()
	content := []byte("private test credential package")
	if err := os.WriteFile(filepath.Join(dir, "codex-model-setup.zip"), content, 0600); err != nil {
		t.Fatal(err)
	}
	h := skillsManualHandler(s, "https://test.trycloudflare.com", filepath.Join(dir, "agent.exe"))
	const path = "/test/codex-model-setup.zip"
	if rec := dbGet(t, h, path); rec.Code != http.StatusSeeOther || strings.Contains(rec.Body.String(), string(content)) {
		t.Fatalf("anonymous download: %d", rec.Code)
	}
	admin := signedIn(t, s)
	_, pw, err := s.dbm.auth.CreateAccount(t.Context(), "model-package-viewer", "", repo.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	viewer := signInAs(t, s, "model-package-viewer", pw)
	if rec := dbGet(t, h, path, viewer); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), string(content)) {
		t.Fatalf("viewer download: %d", rec.Code)
	}
	rec := dbGet(t, h, path, admin)
	if rec.Code != http.StatusOK || rec.Body.String() != string(content) {
		t.Fatalf("admin download: %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store, private" {
		t.Fatal("credential download must not be cached")
	}
	if rec := dbPost(t, h, path, url.Values{}, admin); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected download method: %d", rec.Code)
	}
}

func TestSkillsManualSetupRequiresAdminAndBindsOnlyTestRoster(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := skillsManualHandler(s, "https://test.trycloudflare.com", "")
	if rec := dbGet(t, h, "/test"); rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated setup: %d", rec.Code)
	}
	admin := signedIn(t, s)
	csrf := csrfFrom(t, s, admin, "/account")
	device, err := s.dbm.store.Devices().EnsureByHostname(t.Context(), "SKILL-TEST-PC")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {"invalid"}, "machine": {device.Hostname}, "windows_user": {"work-test"}}
	if rec := dbPost(t, h, "/test/bind", form, admin); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("setup accepted invalid CSRF")
	}
	if _, err := s.dbm.store.Employees().ByWindowsUser(t.Context(), "work-test"); err == nil {
		t.Fatal("invalid setup created an employee")
	}
	_, pw, err := s.dbm.auth.CreateAccount(t.Context(), "test-viewer", "", repo.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	viewer := signInAs(t, s, "test-viewer", pw)
	if rec := dbGet(t, h, "/test", viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer setup: %d", rec.Code)
	}
	form.Set("csrf", csrf)
	if rec := dbPost(t, h, "/test/bind", form, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer binding: %d", rec.Code)
	}
	rec := dbPost(t, h, "/test/bind", form, admin)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("admin binding: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	e, err := s.dbm.store.Employees().ByWindowsUser(t.Context(), "work-test")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := s.dbm.store.Bindings().Open(t.Context(), device.ID)
	if err != nil || binding.EmployeeID != e.ID {
		t.Fatalf("binding: %+v %v", binding, err)
	}
}
