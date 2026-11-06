package adminweb

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/pquerna/otp/totp"
)

var reNewAdminPassword = regexp.MustCompile(`初始密碼：<code class="id">([^<]+)`)

// createOperator opens an account through the page, the way it is really done.
func createOperator(t *testing.T, s *Server, by *http.Cookie, username, email string) string {
	t.Helper()
	rec := dbPost(t, s.Handler(), "/admins/create", url.Values{"csrf": {csrfFrom(t, s, by, "/admins")}, "username": {username}, "email": {email}, "role": {"admin"}}, by)
	m := reNewAdminPassword.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no password on the page: %d", rec.Code)
	}
	return m[1]
}

func customerSignIn(t *testing.T, h http.Handler, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/customer/login", nil))
	csrf := cookieNamed(page, customerLoginCSRFCookie)
	form := url.Values{"csrf": {csrf.Value}, "email": {email}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/customer/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCustomersAndOperatorsComeInSeparately(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	for path, want := range map[string]string{"/": "/customer/login", "/overview": opsSignInPath, "/admins": opsSignInPath, "/login": opsSignInPath, "/customer": "/customer/login"} {
		if rec := dbGet(t, h, path); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("%s: %d -> %q, want %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	if page := dbGet(t, h, opsSignInPath); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="/login"`) {
		t.Fatalf("the operators' sign-in: %d", page.Code)
	}

	customer := customerCookieFor(t, s, "owner@example.com", "manager")
	page := dbGet(t, h, "/customer/login").Body.String()
	for _, path := range []string{"/customer", "/customer?view=new"} {
		page += dbGet(t, h, path, customer).Body.String()
	}
	for _, leak := range []string{opsSignInPath, `action="/login"`, `href="/"`, "運維", "管理控制台", "用管理員帳號登入"} {
		if strings.Contains(page, leak) {
			t.Errorf("a customer's page mentions %q", leak)
		}
	}

	root := signedIn(t, s)
	if rec := dbGet(t, h, opsSignInPath, root); rec.Header().Get("Location") != "/overview" {
		t.Errorf("signed in, the sign-in page went to %q", rec.Header().Get("Location"))
	}
	if rec := dbGet(t, h, "/", root); rec.Header().Get("Location") != "/overview" {
		t.Errorf("signed in, the root went to %q", rec.Header().Get("Location"))
	}
	if rec := dbPost(t, h, "/logout", url.Values{"csrf": {csrfFrom(t, s, root, "/settings")}}, root); rec.Header().Get("Location") != opsSignInPath {
		t.Errorf("signing out went to %q", rec.Header().Get("Location"))
	}
}

func TestNeitherKindOfAccountWorksAtTheOtherEntry(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	root := signedIn(t, s)
	password := createOperator(t, s, root, "夢寒", "menghan@example.com")

	// Enrolling sends them back to their own sign-in, not the customers'.
	enrol := cookieNamed(dbPost(t, h, "/login", url.Values{"username": {"夢寒"}, "password": {password}}), enrolCookie)
	if enrol == nil {
		t.Fatal("no enrolment for a new operator")
	}
	secret := reSecret.FindStringSubmatch(dbGet(t, h, "/enrol", enrol).Body.String())
	if secret == nil {
		t.Fatal("no secret on the enrolment page")
	}
	code, _ := totp.GenerateCode(secret[1], time.Now())
	done := dbPost(t, h, "/enrol", url.Values{"code": {code}}, enrol).Body.String()
	if !strings.Contains(done, `href="`+opsSignInPath+`"`) || strings.Contains(done, `href="/"`) {
		t.Error("the enrolment page does not lead back to the operators' sign-in")
	}

	hash, err := authn.HashPassword("a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.dbm.db.Tickets().CreateContactWithRole(t.Context(), "cust@example.com", "客戶", hash, "employee"); err != nil {
		t.Fatal(err)
	}
	rec := dbPost(t, h, "/login", url.Values{"username": {"cust@example.com"}, "password": {"a-long-enough-password"}})
	if cookieNamed(rec, sessionCookie) != nil || cookieNamed(rec, enrolCookie) != nil {
		t.Fatalf("a customer signed in as staff: %d", rec.Code)
	}
	for _, email := range []string{"menghan@example.com", "夢寒"} {
		if rec := customerSignIn(t, h, email, password); cookieNamed(rec, customerCookie) != nil {
			t.Fatalf("an operator signed in as a customer with %q", email)
		}
	}
}

func TestAnOperatorNamedForThemselvesIsWhoTheRecordsShow(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	root := signedIn(t, s)
	const name = "peng wei 夢寒" // a space and any language; user names are kept in lower case
	operator := signInAs(t, s, name, createOperator(t, s, root, name, "menghan@example.com"))

	rec := createAdminTicket(t, h, operator, csrfFrom(t, s, operator, "/settings"), map[string]string{"request_kind": "other-problem", "problem": "處理人測試"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	id := strings.SplitN(rec.Header().Get("Location"), "id=", 2)[1]
	events, err := s.dbm.db.Tickets().Events(t.Context(), id, "")
	if err != nil || len(events) == 0 || events[0].ActorName != name {
		t.Errorf("the ticket's record: %+v %v", events, err)
	}

	admin, err := s.dbm.store.Admins().ByUsername(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/audit", "/audit?actor=" + url.QueryEscape(name)} {
		page := dbGet(t, h, path, operator).Body.String()
		if !strings.Contains(page, `title="`+admin.ID+`">`+name) {
			t.Errorf("%s does not name the operator for what was recorded under their id", path)
		}
	}
	csv := dbGet(t, h, "/audit.csv?actor="+url.QueryEscape(name), operator)
	if csv.Code == http.StatusOK && strings.Contains(csv.Body.String(), admin.ID) {
		t.Error("the export shows the id instead of the name")
	}
}

func TestAnEnrolmentLinkSurvivesAPersonsName(t *testing.T) {
	uri := enrolmentURI("AI 環境管理", "TEENet 夢寒", "ABC")
	if strings.ContainsAny(uri, " 夢") {
		t.Fatalf("not escaped: %s", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/AI 環境管理:TEENet 夢寒" && u.Host+u.Path != "totp/AI 環境管理:TEENet 夢寒" {
		t.Errorf("label %q %q", u.Host, u.Path)
	}
	if got := u.Query().Get("issuer"); got != "AI 環境管理" {
		t.Errorf("issuer %q", got)
	}
}

func TestOnlyAdminAndViewerAccountsCanBeCreated(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	root := signedIn(t, s)
	for _, role := range []string{"operator", "security", "root"} {
		username := "retired-" + role
		dbPost(t, h, "/admins/create", url.Values{"csrf": {csrfFrom(t, s, root, "/admins")}, "username": {username}, "role": {role}}, root)
		if _, err := s.dbm.store.Admins().ByUsername(t.Context(), username); err == nil {
			t.Errorf("an account with role %s was created", role)
		}
	}
	page := dbGet(t, h, "/admins", root).Body.String()
	if strings.Contains(page, `value="operator"`) || strings.Contains(page, `value="security"`) {
		t.Error("the form still offers a retired role")
	}
}
