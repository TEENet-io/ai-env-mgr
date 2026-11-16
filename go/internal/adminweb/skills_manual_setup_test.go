package adminweb

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

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
