package adminweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// newDatabaseServer is the console in database mode over a test database and
// a fake bucket. It skips without TEST_PG_DSN like the other integration
// tests.
func newDatabaseServer(t *testing.T) (*Server, *fakeStore) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN is not set; skipping the PostgreSQL integration tests")
	}
	ctx := context.Background()
	dsn, err := dbstore.TestDatabaseDSN(ctx, dsn, "aienv_test_adminweb")
	if err != nil {
		t.Fatalf("test database: %v", err)
	}
	database, err := dbstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := database.Pool().Exec(ctx, `drop schema public cascade; create schema public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := database.Pool().Exec(ctx, `grant all on schema public to public`); err != nil {
		t.Fatalf("restore schema grant: %v", err)
	}
	database.Close()

	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte(`{"current":"k1","keys":{"k1":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}}`), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_ = os.Chmod(keyFile, 0o600)

	fs := newFakeStore()
	s, err := NewWithStore(Options{
		Listen: "127.0.0.1:0", Bucket: "ai-collect-sg", Endpoint: "oss-ap-southeast-1.aliyuncs.com",
		Database: &DatabaseOptions{
			DSN: dsn, MasterKeyFile: keyFile,
			OSSAccessKeyID: "server-key", OSSAccessKeySecret: "server-secret",
			Worker: false, SpoolDir: filepath.Join(t.TempDir(), "spool"),
		},
	}, func(config.Config) (store, error) { return fs, nil })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.dbm.db.Close)
	return s, fs
}

// knownAdmin gives the first administrator a password the test knows.
func knownAdmin(t *testing.T, s *Server) (username, password string) {
	t.Helper()
	ctx := context.Background()
	admins, err := s.dbm.store.Admins().List(ctx)
	if err != nil || len(admins) != 1 {
		t.Fatalf("admins = %v (%v), want the one created at first start", admins, err)
	}
	password = "a-password-the-test-knows"
	hash, err := authn.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := s.dbm.store.Admins().SetPasswordHash(ctx, admins[0].ID, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	return admins[0].Username, password
}

func dbPost(t *testing.T, h http.Handler, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func dbGet(t *testing.T, h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	return nil
}

var (
	reSecret = regexp.MustCompile(`密钥（手动输入用）：<code class="id">([A-Z2-7]+)</code>`)
	reCSRF   = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
)

// The whole road in: first start creates an account, the first sign-in leads
// to the authenticator, and only after that does a session open.
func TestDatabaseModeSignInEnrolsThenOpensASession(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	username, password := knownAdmin(t, s)

	// Not signed in: pages redirect to the sign-in form, which asks for an
	// account rather than an AccessKey.
	if rec := dbGet(t, h, "/overview"); rec.Code != http.StatusSeeOther {
		t.Fatalf("/overview unauthenticated = %d, want 303", rec.Code)
	}
	if rec := dbGet(t, h, "/"); !strings.Contains(rec.Body.String(), `name="username"`) {
		t.Fatal("the sign-in page does not ask for a user name")
	}

	// Right password, no authenticator yet: sent to enrolment, not into the
	// console, and no session cookie is issued.
	rec := dbPost(t, h, "/login", url.Values{"username": {username}, "password": {password}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/enrol" {
		t.Fatalf("first sign-in = %d -> %q, want 303 to /enrol", rec.Code, rec.Header().Get("Location"))
	}
	if cookieNamed(rec, sessionCookie) != nil {
		t.Fatal("a session was opened before the authenticator was set up")
	}
	enrol := cookieNamed(rec, enrolCookie)
	if enrol == nil {
		t.Fatal("no enrolment cookie was issued")
	}

	rec = dbGet(t, h, "/enrol", enrol)
	if rec.Code != http.StatusOK {
		t.Fatalf("/enrol = %d", rec.Code)
	}
	m := reSecret.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("the enrolment page shows no secret:\n%s", rec.Body.String())
	}
	secret := m[1]

	// A wrong code does not enrol.
	rec = dbPost(t, h, "/enrol", url.Values{"code": {"000000"}}, enrol)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("enrolment with a wrong code = %d, want 401", rec.Code)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp: %v", err)
	}
	rec = dbPost(t, h, "/enrol", url.Values{"code": {code}}, enrol)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "恢复码") {
		t.Fatalf("enrolment = %d; body lacks the recovery codes", rec.Code)
	}

	// Now a real sign-in: password plus code opens a session.
	rec = dbPost(t, h, "/login", url.Values{"username": {username}, "password": {password}})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in without a code after enrolment = %d, want 401", rec.Code)
	}
	code, _ = totp.GenerateCode(secret, time.Now())
	rec = dbPost(t, h, "/login", url.Values{"username": {username}, "password": {password}, "code": {code}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/overview" {
		t.Fatalf("sign-in = %d -> %q, want 303 to /overview", rec.Code, rec.Header().Get("Location"))
	}
	session := cookieNamed(rec, sessionCookie)
	if session == nil {
		t.Fatal("no session cookie")
	}
	// The cookie is not the database's key for the session.
	if _, _, err := s.dbm.store.Admins().SessionByToken(context.Background(), []byte(session.Value)); err == nil {
		t.Fatal("the session is stored under the raw cookie value")
	}

	for _, path := range []string{"/overview", "/users", "/sites", "/settings", "/releases", "/tasks", "/admins", "/account"} {
		rec := dbGet(t, h, path, session)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), username) {
			t.Errorf("%s does not show who is signed in", path)
		}
	}
	if rec := dbGet(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d", rec.Code)
	}

	// Signing out ends the session for good.
	rec = dbPost(t, h, "/logout", nil, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", rec.Code)
	}
	if rec := dbGet(t, h, "/overview", session); rec.Code != http.StatusSeeOther {
		t.Errorf("the session survived signing out: %d", rec.Code)
	}
}

// signedIn does the enrolment dance and returns a live session cookie.
func signedIn(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	h := s.Handler()
	username, password := knownAdmin(t, s)
	rec := dbPost(t, h, "/login", url.Values{"username": {username}, "password": {password}})
	enrol := cookieNamed(rec, enrolCookie)
	if enrol == nil {
		t.Fatal("no enrolment cookie")
	}
	m := reSecret.FindStringSubmatch(dbGet(t, h, "/enrol", enrol).Body.String())
	if m == nil {
		t.Fatal("no secret on the enrolment page")
	}
	code, _ := totp.GenerateCode(m[1], time.Now())
	if rec := dbPost(t, h, "/enrol", url.Values{"code": {code}}, enrol); rec.Code != http.StatusOK {
		t.Fatalf("enrol = %d", rec.Code)
	}
	code, _ = totp.GenerateCode(m[1], time.Now())
	rec = dbPost(t, h, "/login", url.Values{"username": {username}, "password": {password}, "code": {code}})
	session := cookieNamed(rec, sessionCookie)
	if session == nil {
		t.Fatalf("sign-in = %d, no session", rec.Code)
	}
	return session
}

func csrfFrom(t *testing.T, s *Server, session *http.Cookie, path string) string {
	t.Helper()
	m := reCSRF.FindStringSubmatch(dbGet(t, s.Handler(), path, session).Body.String())
	if m == nil {
		t.Fatalf("no CSRF token on %s", path)
	}
	return m[1]
}

// An onboarding through the form lands in the tables and queues the work,
// with the signed-in administrator on the audit row.
func TestDatabaseModeOnboardingIsATransactionWithQueuedWork(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	session := signedIn(t, s)
	csrf := csrfFrom(t, s, session, "/users")

	rec := dbPost(t, h, "/users/onboard", url.Values{
		"csrf": {csrf}, "windowsUser": {"Work1"}, "name": {"张三"}, "department": {"研发"},
		"budget": {"50"}, "rpm": {"60"}, "tpm": {"2000000"}, "parallel": {"8"},
		"models": {"claude-4.5-sonnet"},
	}, session)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok=1") {
		t.Fatalf("onboard = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	ctx := context.Background()
	employee, err := s.dbm.store.Employees().ByWindowsUser(ctx, "work1")
	if err != nil {
		t.Fatalf("the employee is not in the database: %v", err)
	}
	tasks, err := s.dbm.store.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) < 2 {
		t.Errorf("%d tasks queued, want the provisioning and the export", len(tasks))
	}
	history, err := s.dbm.store.Audit().ByTarget(ctx, "employee", employee.ID, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	admins, _ := s.dbm.store.Admins().List(ctx)
	if len(history) != 1 || history[0].ActorID != admins[0].Username {
		t.Errorf("audit = %+v, want one row by the signed-in administrator", history)
	}

	// The list shows the new account, and the detail page its history.
	if body := dbGet(t, h, "/users", session).Body.String(); !strings.Contains(body, "work1") {
		t.Error("the account list does not show the new employee")
	}
	if body := dbGet(t, h, "/users/detail?user=work1", session).Body.String(); !strings.Contains(body, "account.onboard") {
		t.Error("the detail page does not show the audit row")
	}

	// A wrong CSRF token is refused.
	rec = dbPost(t, h, "/users/offboard", url.Values{"csrf": {"nope"}, "windowsUser": {"work1"}}, session)
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Error("a POST with a bad CSRF token was accepted")
	}
	after, _ := s.dbm.store.Employees().ByWindowsUser(ctx, "work1")
	if !after.Active() {
		t.Error("the refused offboarding went through")
	}
}

func TestDatabaseModePolicyEditsAndMachinesRender(t *testing.T) {
	s, fs := newDatabaseServer(t)
	h := s.Handler()
	session := signedIn(t, s)
	csrf := csrfFrom(t, s, session, "/sites")

	rec := dbPost(t, h, "/sites/mutate", url.Values{"csrf": {csrf}, "add": {"example.org"}}, session)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok=1") {
		t.Fatalf("sites/mutate = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if body := dbGet(t, h, "/sites", session).Body.String(); !strings.Contains(body, "example.org") {
		t.Error("the sites page does not show the added domain")
	}
	// The export for it is queued; nothing was written to OSS by the request.
	if _, ok := fs.objects["agent_workdir/policy.json"]; ok && strings.Contains(string(fs.objects["agent_workdir/policy.json"]), "example.org") {
		t.Error("the request wrote the policy object itself instead of queueing it")
	}

	// A machine bound through the form appears on the overview.
	csrf = csrfFrom(t, s, session, "/users")
	dbPost(t, h, "/users/onboard", url.Values{
		"csrf": {csrf}, "windowsUser": {"work1"}, "budget": {"50"}, "rpm": {"60"}, "tpm": {"2000000"}, "parallel": {"8"},
	}, session)
	rec = dbPost(t, h, "/machines/bind", url.Values{"csrf": {csrf}, "machine": {"DESKTOP-01"}, "user": {"work1"}}, session)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok=1") {
		t.Fatalf("bind = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	body := dbGet(t, h, "/overview", session).Body.String()
	if !strings.Contains(body, "DESKTOP-01") || !strings.Contains(body, "work1") {
		t.Error("the overview does not show the bound machine")
	}

	// Administrators: create one, see the password once.
	csrf = csrfFrom(t, s, session, "/admins")
	// The password is shown in the response to the POST and never put in a
	// URL, where history, proxy logs and the Referer would keep it.
	rec = dbPost(t, h, "/admins/create", url.Values{"csrf": {csrf}, "username": {"Li"}, "role": {"operator"}}, session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "初始密码") {
		t.Fatalf("admins/create = %d; the generated password is not shown", rec.Code)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "pw=") {
		t.Errorf("the password is in a redirect: %q", loc)
	}
	li, err := s.dbm.store.Admins().ByUsername(context.Background(), "li")
	if err != nil || li.Role != repo.RoleOperator {
		t.Errorf("li = %+v (%v)", li, err)
	}
}

func TestDeletedAccountsLeaveTheListUntilAskedFor(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	csrf := csrfFrom(t, s, cookie, "/users")
	form := func(extra url.Values) url.Values {
		v := url.Values{"csrf": {csrf}, "windowsUser": {"work5"}}
		for k, vals := range extra {
			v[k] = vals
		}
		return v
	}
	if rec := dbPost(t, h, "/users/onboard", form(url.Values{"budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}), cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("onboard: %d %s", rec.Code, rec.Body.String())
	}
	// Open accounts cannot be deleted, even with the name typed.
	rec := dbPost(t, h, "/users/delete", form(url.Values{"confirm": {"work5"}}), cookie)
	if rec.Code == http.StatusSeeOther && !strings.Contains(rec.Header().Get("Location"), "err") {
		if list := dbGet(t, h, "/users", cookie); !strings.Contains(list.Body.String(), "work5") {
			t.Fatal("an open account was deleted")
		}
	}
	if rec := dbPost(t, h, "/users/offboard", form(nil), cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("offboard: %d", rec.Code)
	}
	// The wrong confirmation does nothing.
	dbPost(t, h, "/users/delete", form(url.Values{"confirm": {"work6"}}), cookie)
	if list := dbGet(t, h, "/users", cookie); !strings.Contains(list.Body.String(), "work5") {
		t.Fatal("a mistyped confirmation deleted the account")
	}
	if rec := dbPost(t, h, "/users/delete", form(url.Values{"confirm": {"work5"}}), cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if list := dbGet(t, h, "/users", cookie); strings.Contains(list.Body.String(), "work5") {
		t.Fatal("the deleted account is still on the default list")
	}
	deleted := dbGet(t, h, "/users?show=deleted", cookie)
	if !strings.Contains(deleted.Body.String(), "work5") || !strings.Contains(deleted.Body.String(), "已删除") {
		t.Fatal("the deleted filter does not show the account")
	}
	if strings.Contains(deleted.Body.String(), `href="/users/detail?user=work5"`) {
		t.Fatal("a deleted account must not link to a detail page")
	}
	// The name is free again.
	if rec := dbPost(t, h, "/users/onboard", form(url.Values{"budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}), cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("re-onboard: %d %s", rec.Code, rec.Body.String())
	}
	if list := dbGet(t, h, "/users?show=active", cookie); !strings.Contains(list.Body.String(), "work5") {
		t.Fatal("the name could not be reused")
	}
}

func TestTheAllBoxMeansNoModelAllowlist(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	csrf := csrfFrom(t, s, cookie, "/users")
	dbPost(t, h, "/users/onboard", url.Values{"csrf": {csrf}, "windowsUser": {"work8"},
		"budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}, "models": {"glm-5"}}, cookie)
	e, err := s.dbm.store.Employees().ByWindowsUser(t.Context(), "work8")
	if err != nil {
		t.Fatal(err)
	}
	if models, _ := s.dbm.store.Employees().Models(t.Context(), e.ID); len(models) != 1 {
		t.Fatalf("after onboarding with one model: %v", models)
	}
	rec := dbPost(t, h, "/users/models", url.Values{"csrf": {csrf}, "windowsUser": {"work8"},
		"all": {"1"}, "models": {"glm-5", "gemini-2.5-pro"}}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("models: %d %s", rec.Code, rec.Body.String())
	}
	if models, _ := s.dbm.store.Employees().Models(t.Context(), e.ID); len(models) != 0 {
		t.Fatalf("the all box must clear the allowlist, got %v", models)
	}
}
