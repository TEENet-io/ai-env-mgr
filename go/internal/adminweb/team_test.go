package adminweb

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestLogFilterKeepsToTheTeam(t *testing.T) {
	cases := []struct {
		f    logFilter
		want string
	}{
		{logFilter{}, "event_type: llm_call"},
		{logFilter{Employee: "peter"}, `event_type: llm_call and employee_id: "emp-peter"`},
		// Before the move the same person was logged without the team.
		{logFilter{Employee: "peter", Team: "acme"},
			`event_type: llm_call and (employee_id: "acme-emp-peter" or employee_id: "emp-peter")`},
		// Other customers' calls share the logstore; only ours are listed.
		{logFilter{Team: "acme"}, `event_type: llm_call and (employee_id: acme-emp-* or employee_id: emp-*)`},
	}
	for _, c := range cases {
		if got := c.f.filterQuery(); got != c.want {
			t.Errorf("%+v:\n got %s\nwant %s", c.f, got, c.want)
		}
	}
}

func TestSettingTheGatewayTeamMovesEveryone(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	csrf := csrfFrom(t, s, cookie, "/settings")

	if page := dbGet(t, h, "/settings", cookie).Body.String(); !strings.Contains(page, `action="/settings/gateway-team"`) {
		t.Fatal("the settings page lacks the gateway team form")
	}
	dbPost(t, h, "/users/onboard", url.Values{"csrf": {csrf}, "windowsUser": {"work1"}, "email": {"t@example.com"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}, cookie)
	e, _ := s.dbm.store.Employees().ByWindowsUser(ctx, "work1")
	cred, _ := s.dbm.store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: e.AuthEpoch, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	s.dbm.store.Grants().Create(ctx, repo.NewGrant{EmployeeID: e.ID, Epoch: e.AuthEpoch, ExternalUser: "emp-work1", KeyAlias: "emp-work1-e1", CredentialID: cred.ID})

	for _, bad := range []string{"", "Acme Corp"} {
		if rec := dbPost(t, h, "/settings/gateway-team", url.Values{"csrf": {csrf}, "version": {"0"}, "team_id": {bad}}, cookie); !strings.Contains(rec.Header().Get("Location"), "err=") {
			t.Fatalf("team %q was accepted", bad)
		}
	}

	// A preview names who would move and changes nothing.
	rec := dbPost(t, h, "/settings/gateway-team", url.Values{"csrf": {csrf}, "version": {"0"}, "team_id": {"acme"}, "preview": {"1"}}, cookie)
	if loc, _ := url.QueryUnescape(rec.Header().Get("Location")); !strings.Contains(loc, "work1") || strings.Contains(loc, "err=") {
		t.Fatalf("preview: %s", loc)
	}
	if got, version, _ := repo.LoadGatewayTeamSettings(ctx, s.dbm.store.Settings()); got.TeamID != "" || version != 0 || s.teamID() != "" {
		t.Fatalf("a preview stored %+v", got)
	}
	if again, _ := s.dbm.store.Employees().ByID(ctx, e.ID); again.AuthEpoch != e.AuthEpoch {
		t.Fatal("a preview re-issued")
	}

	// Saving stores the team, takes effect at once, and re-issues.
	rec = dbPost(t, h, "/settings/gateway-team", url.Values{"csrf": {csrf}, "version": {"0"}, "team_id": {"acme"}}, cookie)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("save: %s", loc)
	}
	if got, _, _ := repo.LoadGatewayTeamSettings(ctx, s.dbm.store.Settings()); got.TeamID != "acme" || s.teamID() != "acme" {
		t.Fatalf("stored %+v, live %q", got, s.teamID())
	}
	if again, _ := s.dbm.store.Employees().ByID(ctx, e.ID); again.AuthEpoch == e.AuthEpoch {
		t.Fatal("the employee was not re-issued")
	}
	if gw := s.newGateway(); gw.Team() != "acme" {
		t.Fatalf("gateway team = %q", gw.Team())
	}
}

// tenantAdmin answers /whoami for team, as tenant-admin does.
func tenantAdmin(t *testing.T, team string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/whoami" || r.Header.Get("Authorization") != "Bearer ta-token" {
			http.Error(w, `{"error":{}}`, http.StatusForbidden)
			return
		}
		io.WriteString(w, `{"team":"`+team+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTheTenantTokenNamesTheTeam(t *testing.T) {
	ta := tenantAdmin(t, "wewe")
	s := &Server{opts: Options{GatewayAdminURL: ta.URL, GatewayAdminKey: "ta-token"}}
	if team, err := s.tenantTeam(""); err != nil || team != "wewe" {
		t.Fatalf("nothing stored: %q %v", team, err)
	}
	if team, err := s.tenantTeam("wewe"); err != nil || team != "wewe" {
		t.Fatalf("same team stored: %q %v", team, err)
	}
	// A token for another customer must not have this console open accounts
	// in that customer's team.
	if _, err := s.tenantTeam("bippo"); err == nil || !strings.Contains(err.Error(), "bippo") {
		t.Fatalf("another team stored: %v", err)
	}
	// tenant-admin down: start on the stored team; tenant-admin checks every
	// request itself once it is back.
	ta.Close()
	if team, err := s.tenantTeam("wewe"); err != nil || team != "wewe" {
		t.Fatalf("tenant-admin down: %q %v", team, err)
	}
}

func TestBehindTenantAdminTheTeamIsNotEditable(t *testing.T) {
	ta := tenantAdmin(t, "acme")
	s, _ := newDatabaseServerWith(t, func(o *Options) {
		o.GatewayURL, o.GatewayAdminURL, o.GatewayAdminKey = "https://gateway.invalid", ta.URL, "ta-token"
	})
	if s.teamID() != "acme" {
		t.Fatalf("team = %q", s.teamID())
	}
	if gw := s.newGateway(); gw.BaseURL != ta.URL || gw.Team() != "acme" {
		t.Fatalf("management goes to %q as %q", gw.BaseURL, gw.Team())
	}
	h := s.Handler()
	cookie := signedIn(t, s)
	csrf := csrfFrom(t, s, cookie, "/settings")
	page := dbGet(t, h, "/settings", cookie).Body.String()
	if !strings.Contains(page, "<code>acme</code>") || strings.Contains(page, `action="/settings/gateway-team"`) {
		t.Fatal("the settings page should show the team without a form")
	}
	rec := dbPost(t, h, "/settings/gateway-team", url.Values{"csrf": {csrf}, "version": {"0"}, "team_id": {"other"}}, cookie)
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("the team was changed behind tenant-admin")
	}
}
