package adminweb

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// fakeStore is enough of admincore.Store for the console's read paths.
type fakeStore struct {
	objects map[string][]byte
	fail    bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{ossclient.PolicyKey(): []byte(`{"syncIntervalMinutes":15}`)}}
}

// Verify is what sign-in authenticates against.
func (f *fakeStore) Verify() error {
	if f.fail {
		return errors.New("access denied")
	}
	return nil
}

func (f *fakeStore) Get(key string) ([]byte, string, error) {
	b, ok := f.objects[key]
	if !ok {
		return nil, "", &notFound{}
	}
	return b, "etag", nil
}
func (f *fakeStore) Put(key string, data []byte) error { f.objects[key] = data; return nil }
func (f *fakeStore) List(string) ([]string, error)     { return nil, nil }
func (f *fakeStore) ListInfo(string) ([]ossclient.ObjectInfo, error) {
	return nil, nil
}
func (f *fakeStore) Delete(key string) error { delete(f.objects, key); return nil }
func (f *fakeStore) SignedURL(string, time.Duration) (string, error) {
	return "https://example.invalid/x", nil
}

type notFound struct{}

func (n *notFound) Error() string { return "not found" }

func newTestServer(t *testing.T, fs *fakeStore) *Server {
	t.Helper()
	s, err := New(Options{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	s.dialOSS = func(config.Config) (store, error) { return fs, nil }
	return s
}

// Long enough that they cannot appear inside a random session id by chance.
// Two-character stand-ins made the cookie assertion below fail roughly once in
// a hundred runs -- a test that cries wolf that often teaches people to ignore
// it.
const (
	testAccessKeyID     = "LTAIzzTESTACCESSKEYID999"
	testAccessKeySecret = "zzTESTACCESSKEYSECRETvalue000111"
)

func signIn(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	form := url.Values{
		"endpoint":        {"oss-ap-southeast-1.aliyuncs.com"},
		"bucket":          {"ai-collect-sg"},
		"accessKeyId":     {testAccessKeyID},
		"accessKeySecret": {testAccessKeySecret},
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sign-in returned %d, want 303", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("sign-in set no session cookie")
	return nil
}

// The whole security model rests on the console not serving credentials in the
// clear, so a non-loopback bind without a certificate must not start at all.
func TestNewRefusesPublicBindWithoutTLS(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8080", ":8080", "203.0.113.7:443", "admin.example.com:443"} {
		if _, err := New(Options{Listen: listen}); err == nil {
			t.Fatalf("%s was accepted without TLS", listen)
		}
	}
	for _, listen := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if _, err := New(Options{Listen: listen}); err != nil {
			t.Fatalf("%s was rejected: %v", listen, err)
		}
	}
	if _, err := New(Options{Listen: "0.0.0.0:443", CertFile: "c.pem", KeyFile: "k.pem"}); err != nil {
		t.Fatalf("public bind with TLS was rejected: %v", err)
	}
}

// The cookie must be an opaque id. If credentials ever leaked into it, every
// proxy log and browser store would hold fleet-wide access.
func TestSessionCookieCarriesNoCredentials(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	c := signIn(t, s)
	for _, secret := range []string{testAccessKeyID, testAccessKeySecret, "ai-collect-sg", "oss-ap-southeast-1"} {
		if strings.Contains(c.Value, secret) {
			t.Fatalf("cookie value contains %q", secret)
		}
	}
	if !c.HttpOnly {
		t.Fatal("session cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie is not SameSite=Strict, which is what blocks cross-site posts")
	}
}

func TestProtectedPagesRequireSession(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	for _, path := range []string{"/machines", "/policy"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s without a session returned %d, want a redirect", path, rec.Code)
		}
	}
}

func TestSignedInRequestReachesPage(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	c := signIn(t, s)
	req := httptest.NewRequest(http.MethodGet, "/policy", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy page returned %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "15") {
		t.Fatal("policy page did not render the sync interval from the store")
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	c := signIn(t, s)
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(c)
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/policy", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatal("the session still worked after logout")
	}
}

func TestSessionsExpire(t *testing.T) {
	store := newSessionStore(30*time.Minute, 12*time.Hour)
	base := time.Now()
	store.nowFunc = func() time.Time { return base }
	id, err := store.create(&admincore.Manager{Store: newFakeStore()}, "b", "e", nil)
	if err != nil {
		t.Fatal(err)
	}
	if store.get(id) == nil {
		t.Fatal("fresh session was not found")
	}
	// Idle past the limit.
	store.nowFunc = func() time.Time { return base.Add(31 * time.Minute) }
	if store.get(id) != nil {
		t.Fatal("idle session was still valid")
	}

	// A session kept busy must still die at the absolute cap: an idle timer
	// alone would let a stolen cookie live indefinitely.
	store.nowFunc = func() time.Time { return base }
	id, err = store.create(&admincore.Manager{Store: newFakeStore()}, "b", "e", nil)
	if err != nil {
		t.Fatal(err)
	}
	for d := 20 * time.Minute; d < 12*time.Hour; d += 20 * time.Minute {
		step := d
		store.nowFunc = func() time.Time { return base.Add(step) }
		if store.get(id) == nil {
			t.Fatalf("session died early at %v despite steady use", step)
		}
	}
	store.nowFunc = func() time.Time { return base.Add(13 * time.Hour) }
	if store.get(id) != nil {
		t.Fatal("session outlived the absolute cap")
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	l := newLoginLimiter(time.Minute, 3)
	base := time.Now()
	l.nowFunc = func() time.Time { return base }
	for i := 0; i < 3; i++ {
		if !l.allow("198.51.100.9") {
			t.Fatalf("attempt %d was blocked too early", i+1)
		}
	}
	if l.allow("198.51.100.9") {
		t.Fatal("the fourth attempt within the window was allowed")
	}
	// A different caller is unaffected.
	if !l.allow("198.51.100.10") {
		t.Fatal("a different address was blocked")
	}
	// The window slides.
	l.nowFunc = func() time.Time { return base.Add(61 * time.Second) }
	if !l.allow("198.51.100.9") {
		t.Fatal("the limit did not expire")
	}
}

// A failed sign-in must not hand the credential back to the browser, where it
// would land in history, proxy logs and any error reporting.
func TestFailedLoginDoesNotEchoCredentials(t *testing.T) {
	store := newFakeStore()
	store.fail = true
	s := newTestServer(t, store)
	form := url.Values{
		"endpoint":        {"oss-ap-southeast-1.aliyuncs.com"},
		"bucket":          {"ai-collect-sg"},
		"accessKeyId":     {"AKIDSECRETVALUE"},
		"accessKeySecret": {"SKSECRETVALUE"},
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad credentials returned %d, want 401", rec.Code)
	}
	for _, secret := range []string{"AKIDSECRETVALUE", "SKSECRETVALUE"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("response echoed %q", secret)
		}
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	h := rec.Header()
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("missing a restrictive CSP")
	}
	if h.Get("X-Frame-Options") != "DENY" {
		t.Fatal("page can be framed")
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Fatal("sign-in page is cacheable")
	}
}

// --behind-proxy exists so a containerised proxy can reach the console over
// the docker bridge, which is not loopback. It must not become a way to serve
// credentials in the clear to the internet.
func TestBehindProxyOnlyRelaxesPrivateAddresses(t *testing.T) {
	private := []string{"172.23.0.1:9080", "10.0.0.5:9080", "192.168.1.9:9080"}
	for _, listen := range private {
		if _, err := New(Options{Listen: listen}); err == nil {
			t.Fatalf("%s was accepted without TLS or --behind-proxy", listen)
		}
		if _, err := New(Options{Listen: listen, BehindProxy: true}); err != nil {
			t.Fatalf("%s with --behind-proxy was rejected: %v", listen, err)
		}
	}
	// Public addresses, and anything that binds every interface, still need a
	// certificate no matter what the operator claims is in front.
	for _, listen := range []string{"0.0.0.0:9080", ":9080", "47.236.115.50:9080", "admin.example.com:9080"} {
		if _, err := New(Options{Listen: listen, BehindProxy: true}); err == nil {
			t.Fatalf("%s was accepted with --behind-proxy but no TLS", listen)
		}
	}
}

// The browser reaches a proxied console over HTTPS, so the cookie must be
// Secure even though this hop is plaintext -- otherwise it would also be sent
// on any plaintext request to the same host.
func TestBehindProxyMarksCookieSecureAndSetsHSTS(t *testing.T) {
	s, err := New(Options{Listen: "172.23.0.1:9080", BehindProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	s.dialOSS = func(config.Config) (store, error) { return newFakeStore(), nil }
	c := signIn(t, s)
	if !c.Secure {
		t.Fatal("session cookie is not Secure behind a TLS-terminating proxy")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS is missing behind a TLS-terminating proxy")
	}
}

// With one shared peer address, per-caller limiting has to come from the
// header the proxy sets, or one noisy client locks everyone out.
func TestBehindProxyRateLimitsPerRealClient(t *testing.T) {
	s, err := New(Options{Listen: "172.23.0.1:9080", BehindProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.RemoteAddr = "172.23.0.2:5000"
	r1.Header.Set("X-Real-IP", "198.51.100.4")
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "172.23.0.2:5001"
	r2.Header.Set("X-Real-IP", "198.51.100.5")
	if s.clientKey(r1) == s.clientKey(r2) {
		t.Fatal("two clients behind the proxy share a rate-limit bucket")
	}

	// Serving directly, the same header must be ignored: it is attacker
	// controlled and would otherwise defeat the limit entirely.
	direct := newTestServer(t, newFakeStore())
	if got := direct.clientKey(r1); got != "172.23.0.2" {
		t.Fatalf("direct serving trusted X-Real-IP, key = %q", got)
	}
}

// A baked-in location must be enforced, not merely pre-filled. The form does
// not show these fields, so a value arriving in them was crafted, and
// honouring it would let the console be aimed at another bucket -- or another
// endpoint entirely.
func TestFixedLocationOverridesThePostedOne(t *testing.T) {
	s, err := New(Options{
		Listen:   "127.0.0.1:0",
		Bucket:   "ai-collect-sg",
		Endpoint: "oss-ap-southeast-1.aliyuncs.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got config.Config
	s.dialOSS = func(cfg config.Config) (store, error) {
		got = cfg
		return newFakeStore(), nil
	}

	form := url.Values{
		"bucket":          {"attacker-bucket"},
		"endpoint":        {"oss.attacker.example"},
		"accessKeyId":     {testAccessKeyID},
		"accessKeySecret": {testAccessKeySecret},
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sign-in returned %d", rec.Code)
	}
	if got.Bucket != "ai-collect-sg" || got.Endpoint != "oss-ap-southeast-1.aliyuncs.com" {
		t.Fatalf("the posted location won: bucket=%q endpoint=%q", got.Bucket, got.Endpoint)
	}
}

// With nothing baked in, the form still has to ask for all four.
func TestUnfixedLocationStillAsks(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `name="bucket"`) {
		t.Fatal("the sign-in form omits the bucket field when nothing is baked in")
	}
}

// CloudFlare rewrites addresses it finds and injects a script to undo it. The
// CSP here allows no script, so that rewrite is permanent: the markers have to
// survive into the output, and the value has to stay escaped.
func TestAccountsAreExcludedFromEmailObfuscation(t *testing.T) {
	got := string(noEmailScan("peter@teenet.io"))
	if !strings.HasPrefix(got, "<!--email_off-->") || !strings.HasSuffix(got, "<!--email_on-->") {
		t.Fatalf("markers missing: %q", got)
	}
	if !strings.Contains(got, "peter@teenet.io") {
		t.Fatalf("address lost: %q", got)
	}
	if noEmailScan("") != "" {
		t.Fatal("an empty value still emitted markers")
	}
	// The roster is data, so the helper has to escape rather than trust it.
	if esc := string(noEmailScan(`<img src=x onerror=alert(1)>`)); strings.Contains(esc, "<img") {
		t.Fatalf("markup passed through unescaped: %q", esc)
	}

	// And the markers must reach the rendered page: html/template drops HTML
	// comments, which is why they are emitted as trusted HTML at all.
	s := newTestServer(t, newFakeStore())
	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "users.html", pageData{
		Users: []model.UserEntry{{WindowsUser: "peter", CodexAccount: "peter@teenet.io", Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "<!--email_off-->peter@teenet.io<!--email_on-->") {
		t.Fatal("the rendered roster lost its email_off markers")
	}
}

// The publish page renders with a Codex version set, and offers no way to
// stage the fleet. Removing the rollout left field references behind in two
// templates, and a template referencing a value that is gone only fails when
// somebody opens the page -- the compiler and the router both stay quiet.
func TestPublishPageRendersWithoutRolloutControls(t *testing.T) {
	store := newFakeStore()
	s := newTestServer(t, store)
	c := signIn(t, s)

	mgr := &admincore.Manager{Store: store}
	if _, err := mgr.PublishCodexUpdate("26.810.52044-b1", []byte("installer")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/rollout", "/policy"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "26.810.52044-b1") {
			t.Fatalf("%s did not render the published version", path)
		}
		if strings.Contains(body, `name="rollout"`) || strings.Contains(body, "灰度") {
			t.Fatalf("%s still offers a rollout control", path)
		}
	}
}

// The machines table renders the Codex column, with the tag that says why a
// machine is not on the published version yet. Template errors -- a field
// reference that no longer resolves, a mis-scoped variable -- surface only
// when the page is executed, which the compiler and the router never do.
func TestMachinesPageRendersCodexColumn(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	fresh := time.Now().UTC().Format(time.RFC3339)
	machines := []admincore.MachineState{
		{Machine: "pc1", Bound: true, Binding: model.Binding{User: "a"},
			Status: model.Status{Machine: "pc1", LastSync: fresh,
				AgentVersion: "1.2.7", CodexVersion: "26.810.52044-b1"}},
		{Machine: "pc2", Bound: true, Binding: model.Binding{User: "b"},
			Status: model.Status{Machine: "pc2", LastSync: fresh,
				AgentVersion: "1.2.6", CodexVersion: "26.803.81509-b1",
				CodexState: agentcore.CodexFailed}},
		// Too old to install at all: no Codex support before 1.2.5.
		{Machine: "pc3", Bound: true, Binding: model.Binding{User: "c"},
			Status: model.Status{Machine: "pc3", LastSync: fresh, AgentVersion: "1.2.4"}},
	}
	policy := model.Policy{CodexVersion: "26.810.52044-b1"}
	data := pageData{CSRF: "t", Nav: "machines", Machines: machines,
		Fleet: summariseFleet(machines), Policy: &policy}

	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "machines.html", data); err != nil {
		t.Fatalf("machines.html: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		"CODEX",
		"26.810.52044-b1", // arrived
		"26.803.81509-b1", // did not
		"安装失败",            // and will not retry
		"待更新",             // the 1.2.4 machine, which never will
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("machines page is missing %q", want)
		}
	}
	// Exactly one machine is pending: the one on the published version must
	// not be tagged, or the column stops meaning anything.
	if n := strings.Count(body, "待更新"); n != 1 {
		t.Fatalf("待更新 appears %d times, want 1", n)
	}
}
