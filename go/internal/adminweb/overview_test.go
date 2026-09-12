package adminweb

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/config"
)

func get(t *testing.T, s *Server, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The fleet, the policy and the gateway are one page, and the point of merging
// them was that an operator sees all three without navigating. A page that
// renders only two of the three sections has lost the merge.
func TestOverviewCarriesAllThreeSections(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	cookie := signIn(t, s)

	rec := get(t, s, "/overview", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("/overview returned %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<h1>总览</h1>",
		"<h2>当前策略</h2>",
		"<h2>模型网关</h2>",
		"<h2>机器</h2>",
		"<th>主机名</th>", // the machine table itself, not just its heading
		"绑定机器",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview is missing %q", want)
		}
	}
}

// The three merged paths are in everyone's bookmarks. They redirect rather
// than 404, and they redirect to the page that replaced them.
func TestMergedPagesRedirectToTheOverview(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	cookie := signIn(t, s)

	for _, path := range []string{"/machines", "/policy", "/gateway"} {
		rec := get(t, s, path, cookie)
		if rec.Code != http.StatusFound {
			t.Errorf("%s returned %d, want 302", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/overview" {
			t.Errorf("%s redirected to %q, want /overview", path, loc)
		}
	}
}

// The gateway lives in another cloud and this one does not. A gateway that
// refuses the connection must cost its own panel and nothing else: the fleet
// and the policy are read from OSS and have no reason to disappear with it.
func TestOverviewSurvivesAnUnreachableGateway(t *testing.T) {
	// A listener that is closed immediately: the port is real, so the dial
	// fails at once rather than hanging until the deadline.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	s, err := New(Options{Listen: "127.0.0.1:0",
		GatewayURL: "http://" + addr, GatewayAdminKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	s.dialOSS = func(config.Config) (store, error) { return fs, nil }
	cookie := signIn(t, s)

	rec := get(t, s, "/overview", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("/overview returned %d with the gateway down, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "网关不可达") {
		t.Error("the gateway panel does not say the gateway is unreachable")
	}
	for _, want := range []string{"<h2>当前策略</h2>", "<h2>机器</h2>"} {
		if !strings.Contains(body, want) {
			t.Errorf("a dead gateway took %q down with it", want)
		}
	}
}

// A gateway that was never configured is not a broken one, and saying
// "unreachable" about a deployment that simply has no gateway would send
// somebody debugging a network that is fine.
func TestOverviewSaysWhenNoGatewayIsConfigured(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	cookie := signIn(t, s)
	body := get(t, s, "/overview", cookie).Body.String()
	if !strings.Contains(body, "未配置网关") {
		t.Error("the gateway panel does not say the gateway is unconfigured")
	}
	if strings.Contains(body, "网关不可达") {
		t.Error("an unconfigured gateway must not read as an unreachable one")
	}
}

// Binding and unbinding are posted from the overview, so that is where the
// redirect afterwards has to land -- a POST that bounced back to the old
// /machines would leave the operator a redirect away from the table they
// just changed.
func TestMachineActionsReturnToTheOverview(t *testing.T) {
	for _, path := range []string{"/machines/bind", "/machines/unbind", "/machines/forget"} {
		s := newTestServer(t, newFakeStore())
		cookie := signIn(t, s)
		form := url.Values{
			"csrf":    {csrfOf(t, s, cookie)},
			"machine": {"pc1"},
			"user":    {"work1"},
			"confirm": {"pc1"},
		}
		rec := post(t, s, path, cookie, form)
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, "/overview") {
			t.Errorf("%s redirected to %q, want /overview", path, loc)
		}
	}
}
