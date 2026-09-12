package adminweb

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

// A publish keeps running while the operator wanders back to the overview,
// and the machine table there is exactly what it is changing. The page
// carries the same meta refresh the rollout page does, in <head> where a
// browser will act on it -- inside <body> it is decoration.
func TestOverviewRefreshesWhileAPublishRuns(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	cookie := signIn(t, s)

	// Quiet: nothing running, so nothing should be reloading either.
	if body := get(t, s, "/overview", cookie).Body.String(); strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Fatal("the overview reloads itself with no publish running")
	}

	s.jobs.current = &job{Kind: "agent", Version: "1.2.8", State: jobRunning,
		Step: "上传到 OSS", Started: time.Now()}

	body := get(t, s, "/overview", cookie).Body.String()
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("the overview does not refresh while a publish is running")
	}
	// In <head>: a meta refresh after </head> is ignored by the browsers this
	// console is used in, which would make the directive look present and do
	// nothing.
	if strings.Index(body, `http-equiv="refresh"`) > strings.Index(body, "<body>") {
		t.Error("the refresh directive is inside <body>, where it has no effect")
	}
}

// A gateway this console cannot authenticate to is a configuration mistake,
// not a network one. Reporting it as 网关不可达 sends somebody to debug a
// network that is fine, so that phrase is reserved for a gateway that was
// reachable enough to try and did not answer.
func TestOverviewSeparatesAMissingKeyFromAnUnreachableGateway(t *testing.T) {
	s, err := New(Options{Listen: "127.0.0.1:0", GatewayURL: "https://gw.example"})
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	s.dialOSS = func(config.Config) (store, error) { return fs, nil }
	cookie := signIn(t, s)

	body := get(t, s, "/overview", cookie).Body.String()
	if strings.Contains(body, "网关不可达") {
		t.Error("a missing admin key must not be reported as an unreachable gateway")
	}
	if !strings.Contains(body, "网关管理密钥未注入") {
		t.Error("the panel does not say what is actually wrong with the configuration")
	}
}

// The policy's UpdatedAt is stored in UTC and read by people eight hours
// ahead of it. Shown raw it is a stamp the reader has to convert.
func TestOverviewShowsThePolicyTimeAsLocalWallClock(t *testing.T) {
	if got := localStamp("2026-08-16T09:03:50Z"); got != "2026-08-16 17:03" {
		t.Errorf("localStamp = %q, want the Shanghai wall clock", got)
	}
	if got := localStamp(""); got != "—" {
		t.Errorf("an empty stamp = %q, want the em dash", got)
	}
	// An unparseable value is shown rather than hidden: it is what is really
	// in the bucket, and hiding it would make a corrupt policy look fine.
	if got := localStamp("not a time"); got != "not a time" {
		t.Errorf("an unparseable stamp = %q, want it shown as stored", got)
	}
}

// An SLS that cannot be queried and an hour with no probes both leave the
// counters at zero. Only one of them means nobody is watching the gateway, so
// the panel has to say which it is rather than letting a read failure pass for
// a quiet hour.
func TestOverviewExplainsAMissingOrPartialProbe(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	render := func(d pageData) string {
		t.Helper()
		d.Nav, d.CSRF, d.GatewayURL = "overview", "t", "https://gw.example"
		var buf bytes.Buffer
		if err := s.tpl.ExecuteTemplate(&buf, "overview.html", d); err != nil {
			t.Fatalf("overview.html: %v", err)
		}
		return buf.String()
	}

	notice := render(pageData{ProbeNotice: "这把 AccessKey 没有日志读取权限"})
	if !strings.Contains(notice, "探测状态读不到") || !strings.Contains(notice, "没有日志读取权限") {
		t.Error("a failed probe query is not explained on the page")
	}

	partial := render(pageData{
		Probe:           &probeHealth{OKs: 5, Sev: "ok", Label: "网关正常"},
		ProbeIncomplete: true,
	})
	if !strings.Contains(partial, "探测数据可能不完整") {
		t.Error("an incomplete index is not disclosed, so a low count reads as the truth")
	}

	// A healthy, complete hour says neither.
	clean := render(pageData{Probe: &probeHealth{OKs: 58, Sev: "ok", Label: "网关正常"}})
	for _, unwanted := range []string{"探测状态读不到", "探测数据可能不完整"} {
		if strings.Contains(clean, unwanted) {
			t.Errorf("a healthy probe still shows %q", unwanted)
		}
	}
}
