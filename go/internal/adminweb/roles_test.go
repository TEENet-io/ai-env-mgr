package adminweb

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRequiredRoleReadsTheTableExactlyThenByPrefix(t *testing.T) {
	cases := map[string]string{
		"GET /overview":         "viewer",
		"GET /users/detail":     "viewer",
		"GET /rollouts/detail":  "viewer",
		"POST /users/offboard":  "operator",
		"POST /releases/global": "operator",
		"POST /machines/sync":   "operator",
		"GET /admins":           "admin",
		"POST /admins/create":   "admin",
		"POST /something/new":   "admin", // unknown: closed
		"GET /nowhere":          "admin",
		"POST /account":         "viewer",
		"POST /logout":          "viewer",
	}
	for req, want := range cases {
		m, p, _ := strings.Cut(req, " ")
		if got := requiredRole(m, p); got != want {
			t.Errorf("%s: %s, want %s", req, got, want)
		}
	}
	if !allowed("security", "GET", "/audit") || allowed("security", "POST", "/users/offboard") {
		t.Error("security reads everything and changes nothing")
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
	// The first administrator makes a viewer and an operator.
	viewer, viewerPassword, err := s.dbm.auth.CreateAccount(ctx, "eve", "", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	operator, operatorPassword, err := s.dbm.auth.CreateAccount(ctx, "olga", "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	_ = viewer
	_ = operator
	viewerCookie := signInAs(t, s, "eve", viewerPassword)
	operatorCookie := signInAs(t, s, "olga", operatorPassword)

	if rec := dbGet(t, h, "/overview", viewerCookie); rec.Code != 200 {
		t.Fatalf("viewer overview: %d", rec.Code)
	}
	if rec := dbGet(t, h, "/audit", viewerCookie); rec.Code != 200 {
		t.Fatalf("viewer audit: %d", rec.Code)
	}
	csrf := csrfFrom(t, s, viewerCookie, "/users")
	onboard := url.Values{"csrf": {csrf}, "windowsUser": {"work1"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}
	if rec := dbPost(t, h, "/users/onboard", onboard, viewerCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer onboard: %d, want 403", rec.Code)
	}
	if rec := dbGet(t, h, "/admins", viewerCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer admins: %d, want 403", rec.Code)
	}

	csrf = csrfFrom(t, s, operatorCookie, "/users")
	onboard.Set("csrf", csrf)
	if rec := dbPost(t, h, "/users/onboard", onboard, operatorCookie); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("operator onboard: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := dbPost(t, h, "/admins/create", url.Values{"csrf": {csrf}, "username": {"x"}}, operatorCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("operator creating an admin: %d, want 403", rec.Code)
	}
	if rec := dbGet(t, h, "/admins", adminCookie); rec.Code != 200 {
		t.Fatalf("admin admins: %d", rec.Code)
	}
	// A viewer can still change their own password.
	if rec := dbGet(t, h, "/account", viewerCookie); rec.Code != 200 {
		t.Fatalf("viewer account page: %d", rec.Code)
	}
}
