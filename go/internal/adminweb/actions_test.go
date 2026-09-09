package adminweb

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// post drives a state-changing route the way a browser would.
func post(t *testing.T, s *Server, path string, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func csrfOf(t *testing.T, s *Server, cookie *http.Cookie) string {
	t.Helper()
	sess := s.sessions.get(cookie.Value)
	if sess == nil {
		t.Fatal("no session for cookie")
	}
	return sess.csrf
}

// Without this, any page on the internet could post a form that retires an
// employee or unbinds a machine using the operator's session.
func TestWritesRequireCSRFToken(t *testing.T) {
	writes := []struct {
		path string
		form url.Values
	}{
		{"/users/onboard", url.Values{"windowsUser": {"mallory"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"200000"}, "parallel": {"4"}}},
		{"/users/reopen", url.Values{"windowsUser": {"work1"}}},
		{"/users/offboard", url.Values{"windowsUser": {"work1"}}},
		{"/users/quota", url.Values{"windowsUser": {"work1"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"200000"}, "parallel": {"4"}}},
		{"/users/models", url.Values{"windowsUser": {"work1"}}},
		{"/users/reissue", url.Values{"windowsUser": {"work1"}}},
		{"/users/profile", url.Values{"windowsUser": {"work1"}, "name": {"Mallory"}}},
		{"/machines/bind", url.Values{"machine": {"PC1"}, "user": {"work1"}}},
		{"/machines/unbind", url.Values{"machine": {"PC1"}}},
		{"/sites/mutate", url.Values{"add": {"evil.example"}}},
		{"/sites/enabled", url.Values{"enabled": {"0"}}},
		{"/sites/applocker", url.Values{"add": {`C:\tools\Codex\*`}}},
		{"/sites/applocker-mode", url.Values{"mode": {"audit"}}},
		{"/settings/interval", url.Values{"minutes": {"1"}}},
		{"/settings/collect", url.Values{"enabled": {"1"}}},
		{"/settings/quota-defaults", url.Values{"budget": {"20"}, "rpm": {"60"}, "tpm": {"200000"}, "parallel": {"4"}}},
	}
	for _, w := range writes {
		fs := newFakeStore()
		s := newTestServer(t, fs)
		cookie := signIn(t, s)
		before := len(fs.objects)

		// A valid session but no token: must not act.
		rec := post(t, s, w.path, cookie, w.form)
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
			t.Fatalf("%s without a CSRF token did not report an error (Location %q)", w.path, loc)
		}
		if len(fs.objects) != before {
			t.Fatalf("%s without a CSRF token still wrote to the store", w.path)
		}

		// A wrong token must fail the same way.
		bad := url.Values{"csrf": {"not-the-token"}}
		for k, v := range w.form {
			bad[k] = v
		}
		rec = post(t, s, w.path, cookie, bad)
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
			t.Fatalf("%s with a wrong CSRF token was accepted", w.path)
		}
		if len(fs.objects) != before {
			t.Fatalf("%s with a wrong CSRF token still wrote to the store", w.path)
		}
	}
}

// splitPaths must not treat a space as a separator: Windows paths routinely
// contain them (C:\Program Files\...), unlike the domains splitDomains
// handles.
func TestSplitPathsPreservesSpacesSplitsOnLinesCommasSemicolons(t *testing.T) {
	got := splitPaths("C:\\a b\\*\nC:\\c\\*, D:\\d\\*")
	want := []string{`C:\a b\*`, `C:\c\*`, `D:\d\*`}
	if len(got) != len(want) {
		t.Fatalf("splitPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitPaths = %v, want %v", got, want)
		}
	}
}

func TestWritesRequireSession(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	rec := post(t, s, "/users/onboard", nil, url.Values{"windowsUser": {"mallory"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("an unauthenticated write returned %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestWriteWithValidTokenApplies(t *testing.T) {
	fs := newFakeStore()
	s := newTestServer(t, fs)
	cookie := signIn(t, s)
	form := url.Values{"csrf": {csrfOf(t, s, cookie)}, "minutes": {"42"}}

	rec := post(t, s, "/settings/interval", cookie, form)
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "?ok=1") {
		t.Fatalf("valid write redirected to %q", loc)
	}
	p, err := (&testManager{fs}).policy()
	if err != nil {
		t.Fatal(err)
	}
	if p.SyncIntervalMinutes != 42 {
		t.Fatalf("interval = %d, want 42", p.SyncIntervalMinutes)
	}
}

// The button an administrator presses to take AppLocker out of enforcement
// must publish the mode, and a value the agent would not understand must be
// refused before it reaches the fleet.
func TestAppLockerModeButtonPublishesTheMode(t *testing.T) {
	fs := newFakeStore()
	s := newTestServer(t, fs)
	cookie := signIn(t, s)
	token := csrfOf(t, s, cookie)

	rec := post(t, s, "/sites/applocker-mode", cookie, url.Values{"csrf": {token}, "mode": {"audit"}})
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "?ok=1") {
		t.Fatalf("valid switch redirected to %q", loc)
	}
	p, err := (&testManager{fs}).policy()
	if err != nil {
		t.Fatal(err)
	}
	if p.AppLockerMode != "audit" {
		t.Fatalf("AppLockerMode = %q, want audit", p.AppLockerMode)
	}

	rec = post(t, s, "/sites/applocker-mode", cookie, url.Values{"csrf": {token}, "mode": {"AuditOnly"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("an unknown mode was accepted (Location %q)", loc)
	}
	if p, err = (&testManager{fs}).policy(); err != nil || p.AppLockerMode != "audit" {
		t.Fatalf("a refused mode must leave the published policy alone: %q %v", p.AppLockerMode, err)
	}
}

// A form posted from the detail page must return to the detail page on
// failure too, not just on success -- and it must be a page that actually
// renders, not a 404 caused by the error query string corrupting the
// "user" parameter already on that URL.
func TestDetailPageErrorRedirectStaysOnDetailPage(t *testing.T) {
	fs := newFakeStore()
	fs.objects[ossclient.AdminKey("users.json")] = []byte(`{"users":[{"windowsUser":"work1","enabled":true}]}`)
	s := newTestServer(t, fs)
	cookie := signIn(t, s)
	form := url.Values{
		"csrf": {csrfOf(t, s, cookie)}, "windowsUser": {"work1"}, "back": {"detail"},
		"budget": {"abc"}, "rpm": {"60"}, "tpm": {"200000"}, "parallel": {"4"},
	}

	rec := post(t, s, "/users/quota", cookie, form)
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/users/detail?user=work1&err=") {
		t.Fatalf("error redirect went to %q", loc)
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := parsed.Query().Get("err")
	if wantErr == "" {
		t.Fatal("redirect carried no err value")
	}

	req := httptest.NewRequest(http.MethodGet, loc, nil)
	req.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("following the error redirect returned %d, want 200", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), wantErr) {
		t.Fatalf("the detail page did not show the error %q: %s", wantErr, rec2.Body.String())
	}
}

// A GET on a write route must not act -- browsers, crawlers and link
// prefetchers all issue GETs unprompted.
func TestWriteRoutesIgnoreGET(t *testing.T) {
	fs := newFakeStore()
	s := newTestServer(t, fs)
	cookie := signIn(t, s)
	before := len(fs.objects)

	req := httptest.NewRequest(http.MethodGet, "/users/onboard?windowsUser=mallory", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET on a write route returned %d", rec.Code)
	}
	if len(fs.objects) != before {
		t.Fatal("GET on a write route changed the store")
	}
}

// The fleet-wide and irreversible actions are reachable now, but every one of
// them must still demand its CSRF token like the rest.
func TestHighRiskActionsStillRequireCSRF(t *testing.T) {
	for _, path := range []string{
		"/agent/publish", "/agent/cancel", "/codex/publish", "/codex/cancel",
		"/machines/forget", "/files/put", "/files/rm",
		"/employee-login/start", "/employee-login/finish",
	} {
		fs := newFakeStore()
		s := newTestServer(t, fs)
		cookie := signIn(t, s)
		before := len(fs.objects)
		rec := post(t, s, path, cookie, url.Values{"version": {"1.0.0"}, "machine": {"PC1"}})
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
			t.Fatalf("%s without a CSRF token did not report an error", path)
		}
		if len(fs.objects) != before {
			t.Fatalf("%s without a CSRF token still wrote to the store", path)
		}
	}
}

// Every TUI menu entry needs a way in from the navigation. The employee
// sign-in page existed for a while with no link to it, which is the same as
// not having built it -- this fails if that happens again.
func TestNavigationCoversEveryTUIEntry(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	cookie := signIn(t, s)
	req := httptest.NewRequest(http.MethodGet, "/machines", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	// One per menu item in cmd/admin/tui.go.
	for _, path := range []string{
		"/machines",       // 机器状态 + 机器管理
		"/users",          // 员工管理
		"/employee-login", // 代员工登录
		"/sites",          // 封禁策略
		"/settings",       // 会话采集
		"/files",          // 文件传输
		"/rollout",        // agent 更新
	} {
		if !strings.Contains(body, `href="`+path+`"`) {
			t.Errorf("the navigation has no link to %s", path)
		}
	}
}
