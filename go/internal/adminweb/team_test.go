package adminweb

import (
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
