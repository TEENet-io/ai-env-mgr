package adminweb

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
		{"/users/add", url.Values{"windowsUser": {"mallory"}}},
		{"/users/enabled", url.Values{"windowsUser": {"work1"}, "enabled": {"0"}}},
		{"/machines/bind", url.Values{"machine": {"PC1"}, "user": {"work1"}}},
		{"/machines/unbind", url.Values{"machine": {"PC1"}}},
		{"/sites/mutate", url.Values{"add": {"evil.example"}}},
		{"/sites/enabled", url.Values{"enabled": {"0"}}},
		{"/settings/interval", url.Values{"minutes": {"1"}}},
		{"/settings/collect", url.Values{"enabled": {"1"}}},
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

func TestWritesRequireSession(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	rec := post(t, s, "/users/add", nil, url.Values{"windowsUser": {"mallory"}})
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

// A GET on a write route must not act -- browsers, crawlers and link
// prefetchers all issue GETs unprompted.
func TestWriteRoutesIgnoreGET(t *testing.T) {
	fs := newFakeStore()
	s := newTestServer(t, fs)
	cookie := signIn(t, s)
	before := len(fs.objects)

	req := httptest.NewRequest(http.MethodGet, "/users/add?windowsUser=mallory", nil)
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
