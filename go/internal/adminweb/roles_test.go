package adminweb

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRequiredRoleReadsTheTableExactlyThenByPrefix(t *testing.T) {
	cases := map[string]string{
		"GET /overview":          "viewer",
		"GET /users/detail":      "viewer",
		"GET /rollouts/detail":   "viewer",
		"POST /users/offboard":   "admin",
		"POST /releases/global":  "admin",
		"POST /apps/delete":      "admin",
		"POST /releases/notes":   "admin",
		"POST /sites/mutate":     "admin",
		"POST /settings/collect": "admin",
		"POST /users/delete":     "admin",
		"POST /users/quota":      "admin",
		"POST /rollouts/create":  "admin",
		"POST /machines/version": "admin",
		"POST /machines/sync":    "admin",
		"GET /admins":            "admin",
		"POST /admins/create":    "admin",
		"POST /something/new":    "admin", // unknown: closed
		"GET /nowhere":           "admin",
		"POST /account":          "viewer",
		"POST /logout":           "viewer",
	}
	for req, want := range cases {
		m, p, _ := strings.Cut(req, " ")
		if got := requiredRole(m, p); got != want {
			t.Errorf("%s: %s, want %s", req, got, want)
		}
	}
	for _, retired := range []string{"operator", "security"} {
		if allowed(retired, "GET", "/overview") {
			t.Errorf("the retired role %s is still allowed in", retired)
		}
	}
	if allowed("", "GET", "/overview") || allowed("bogus", "GET", "/overview") {
		t.Error("an unknown role is allowed nothing")
	}
}

func TestRolesGateWrites(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	adminCookie := signedIn(t, s)
	ctx := t.Context()
	// The first administrator makes a viewer.
	viewer, viewerPassword, err := s.dbm.auth.CreateAccount(ctx, "eve", "", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	_ = viewer
	viewerCookie := signInAs(t, s, "eve", viewerPassword)

	if rec := dbGet(t, h, "/overview", viewerCookie); rec.Code != 200 {
		t.Fatalf("viewer overview: %d", rec.Code)
	}
	if rec := dbGet(t, h, "/audit", viewerCookie); rec.Code != 200 {
		t.Fatalf("viewer audit: %d", rec.Code)
	}
	csrf := csrfFrom(t, s, viewerCookie, "/users")
	onboard := url.Values{"csrf": {csrf}, "windowsUser": {"work1"}, "email": {"t@example.com"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}
	if rec := dbPost(t, h, "/users/onboard", onboard, viewerCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer onboard: %d, want 403", rec.Code)
	}
	if rec := dbGet(t, h, "/admins", viewerCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer admins: %d, want 403", rec.Code)
	}

	csrf = csrfFrom(t, s, adminCookie, "/users")
	onboard.Set("csrf", csrf)
	if rec := dbPost(t, h, "/users/onboard", onboard, adminCookie); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("admin onboard: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := dbGet(t, h, "/admins", adminCookie); rec.Code != 200 {
		t.Fatalf("admin admins: %d", rec.Code)
	}
	// A viewer can still change their own password.
	if rec := dbGet(t, h, "/account", viewerCookie); rec.Code != 200 {
		t.Fatalf("viewer account page: %d", rec.Code)
	}
}

// A viewer is refused fleet-wide changes on the server and does not see
// their buttons.
func TestAViewerCannotChangeFleetWideThings(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	ctx := t.Context()
	admin := signedIn(t, s)
	_, password, err := s.dbm.auth.CreateAccount(ctx, "olga", "", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	cookie := signInAs(t, s, "olga", password)
	csrf := csrfFrom(t, s, cookie, "/users")
	for path, form := range map[string]url.Values{
		"/sites/mutate":            {"op": {"add"}, "domain": {"example.com"}},
		"/sites/enabled":           {"enabled": {"0"}},
		"/settings/quota-defaults": {"email": {"t@example.com"}, "budget": {"50"}},
		"/settings/interval":       {"minutes": {"5"}},
		"/releases/global":         {"product": {"agent"}, "version": {"1.3.0"}, "confirm": {"1.3.0"}},
		"/releases/global-clear":   {"product": {"agent"}},
		"/users/delete":            {"windowsUser": {"x"}, "confirm": {"x"}},
	} {
		form.Set("csrf", csrf)
		if rec := dbPost(t, h, path, form, cookie); rec.Code != http.StatusForbidden {
			t.Errorf("viewer POST %s: %d, want 403", path, rec.Code)
		}
	}
	settings := dbGet(t, h, "/settings", cookie).Body.String()
	if !strings.Contains(settings, "需要管理員") || !strings.Contains(settings, `<fieldset class="bare" disabled>`) {
		t.Error("a viewer's settings page should be read-only and say why")
	}
	if strings.Contains(dbGet(t, h, "/releases", cookie).Body.String(), `action="/releases/global"`) {
		t.Error("a viewer should not be offered 設為全局")
	}
	// The admin still has them.
	if strings.Contains(dbGet(t, h, "/settings", admin).Body.String(), "需要管理員") {
		t.Error("an admin's settings page must not be read-only")
	}
}
