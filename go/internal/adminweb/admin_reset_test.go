package adminweb

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

var reResetPassword = regexp.MustCompile(`新密碼：<code class="id">([^<]+)`)

// A forgotten password or a lost phone is fixed by another administrator:
// a new password, the old authenticator gone, the old sessions ended.
func TestAnAdministratorResetsAnotherOnesSignIn(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	rootName, _ := knownAdmin(t, s)
	root := signedIn(t, s)
	oldPassword := createOperator(t, s, root, "zhang", "zhang@example.com")
	zhang := signInAs(t, s, "zhang", oldPassword)

	reset := func(by *http.Cookie, username, confirm string) *http.Response {
		rec := dbPost(t, h, "/admins/reset", url.Values{"csrf": {csrfFrom(t, s, by, "/admins")}, "username": {username}, "confirm": {confirm}}, by)
		return rec.Result()
	}

	// The confirmation has to name the account; nothing changes otherwise.
	if res := reset(root, "zhang", "li"); res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "err=") {
		t.Fatalf("a mismatched confirmation = %d -> %q", res.StatusCode, res.Header.Get("Location"))
	}
	if rec := dbGet(t, h, "/overview", zhang); rec.Code != http.StatusOK {
		t.Fatalf("a refused reset ended the session: %d", rec.Code)
	}
	// Nobody resets themselves: that would swap the second factor for a
	// password alone.
	if res := reset(root, rootName, rootName); !strings.Contains(res.Header.Get("Location"), "err=") {
		t.Fatalf("an administrator reset themselves: %d -> %q", res.StatusCode, res.Header.Get("Location"))
	}

	rec := dbPost(t, h, "/admins/reset", url.Values{"csrf": {csrfFrom(t, s, root, "/admins")}, "username": {"zhang"}, "confirm": {"ZHANG"}}, root)
	m := reResetPassword.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("reset = %d, no new password on the page", rec.Code)
	}
	newPassword := m[1]
	if newPassword == oldPassword {
		t.Fatal("the reset kept the old password")
	}

	if rec := dbGet(t, h, "/overview", zhang); rec.Code != http.StatusSeeOther {
		t.Errorf("a session survived the reset: %d", rec.Code)
	}
	if rec := dbPost(t, h, "/login", url.Values{"username": {"zhang"}, "password": {oldPassword}}); cookieNamed(rec, enrolCookie) != nil || cookieNamed(rec, sessionCookie) != nil {
		t.Error("the old password still signs in")
	}
	// The new password leads to enrolment again, and from there into the console.
	if rec := dbPost(t, h, "/login", url.Values{"username": {"zhang"}, "password": {newPassword}}); rec.Header().Get("Location") != "/enrol" {
		t.Fatalf("after a reset, sign-in went to %q, want enrolment", rec.Header().Get("Location"))
	}
	signInAs(t, s, "zhang", newPassword)

	admin, err := s.dbm.store.Admins().ByUsername(t.Context(), "zhang")
	if err != nil {
		t.Fatal(err)
	}
	if !admin.TOTPEnrolled() || len(admin.RecoveryHashes) == 0 {
		t.Error("re-enrolment after the reset did not store a new authenticator")
	}
}

func TestAViewerCannotResetAnAdministrator(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	rootName, _ := knownAdmin(t, s)
	root := signedIn(t, s)
	_, viewerPassword, err := s.dbm.auth.CreateAccount(t.Context(), "eve", "", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	viewer := signInAs(t, s, "eve", viewerPassword)
	rec := dbPost(t, h, "/admins/reset", url.Values{"csrf": {csrfFrom(t, s, viewer, "/settings")}, "username": {rootName}, "confirm": {rootName}}, viewer)
	if reResetPassword.MatchString(rec.Body.String()) {
		t.Fatal("a viewer reset an administrator")
	}
	if dbGet(t, h, "/overview", root).Code != http.StatusOK {
		t.Error("the administrator's session ended after a viewer's attempt")
	}
}

// Account changes are in the audit trail the audit page searches, and they
// name the account by id, so a renamed account is still the one shown.
func TestAccountChangesAreInTheAuditTrailUnderTheAccountsID(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	rootName, _ := knownAdmin(t, s)
	root := signedIn(t, s)
	password := createOperator(t, s, root, "zhang", "zhang@example.com")
	signInAs(t, s, "zhang", password)
	dbPost(t, h, "/admins/reset", url.Values{"csrf": {csrfFrom(t, s, root, "/admins")}, "username": {"zhang"}, "confirm": {"zhang"}}, root)
	dbPost(t, h, "/admins/disable", url.Values{"csrf": {csrfFrom(t, s, root, "/admins")}, "username": {"zhang"}}, root)

	zhang, err := s.dbm.store.Admins().ByUsername(t.Context(), "zhang")
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := s.dbm.store.Audit().Search(t.Context(), repo.AuditFilter{TargetType: "admin", TargetID: zhang.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ev := range events {
		got[ev.Action] = ev.ActorID
	}
	for action, actor := range map[string]string{
		"admin.create": rootName, "admin.enrol": "zhang", "admin.signin": "zhang",
		"admin.reset": rootName, "admin.disable": rootName,
	} {
		if got[action] != actor {
			t.Errorf("%s by %q, want by %q (all: %v)", action, got[action], actor, got)
		}
	}
	for _, ev := range events {
		if strings.Contains(string(ev.Detail), "password") {
			t.Errorf("%s detail mentions a password: %s", ev.Action, ev.Detail)
		}
	}

	if _, err := s.dbm.db.Pool().Exec(t.Context(), `update admin_principals set username = 'zhang2' where id = $1`, zhang.ID); err != nil {
		t.Fatal(err)
	}
	page := dbGet(t, h, "/audit?action=admin.reset", root).Body.String()
	if !strings.Contains(page, "zhang2") {
		t.Error("the audit page does not show the account under its current name")
	}
}
