package adminweb

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/slsclient"
)

// The query expression is the whole of the access control on this page: it is
// the only thing standing between a URL parameter and the logstore. Pinned as
// exact text so a change to how filters are built has to be deliberate.
func TestFilterQueryIsExactText(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    logFilter
		want string
	}{
		{"no filter", logFilter{}, "event_type: llm_call"},
		{"employee", logFilter{Employee: "peter"},
			`event_type: llm_call and employee_id: "emp-peter"`},
		{"status", logFilter{Status: "failure"},
			"event_type: llm_call and status: failure"},
		{"model", logFilter{Model: "glm-5"},
			`event_type: llm_call and model_group: "glm-5"`},
		{"all three", logFilter{Employee: "work1", Status: "success", Model: "grok-4.6"},
			`event_type: llm_call and employee_id: "emp-work1" and status: success and model_group: "grok-4.6"`},
	} {
		if got := tc.f.filterQuery(); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// The summary must dedupe by event_id, or a replayed collection bills an
// employee twice for one call.
func TestSummaryQueryDedupesByEventID(t *testing.T) {
	got := logFilter{Employee: "peter"}.summaryQuery()
	want := `event_type: llm_call and employee_id: "emp-peter"` +
		` | select count(*) as calls, count_if(status='failure') as failures,` +
		` count_if(status='cancelled') as cancelled,` +
		` round(sum(try_cast(cost_usd as double)),4) as cost` +
		` from (select distinct event_id, status, cost_usd from log)`
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

// A typo in a hand-edited URL degrades to the default. It must never reach
// the query expression, and it must never produce an error page either.
func TestParseLogFilterDropsAnythingUnrecognised(t *testing.T) {
	q := url.Values{
		"employee": {`peter" or employee_id: "work1`},
		"status":   {"SUCCESS"},          // the allow-list is exact, not case-insensitive
		"model":    {"glm 5 | select *"}, // spaces and a pipe
		"range":    {"forever"},
		"page":     {"-3"},
	}
	f := parseLogFilter(q)
	if f.Employee != "" || f.Status != "" || f.Model != "" {
		t.Fatalf("a bad value survived: %+v", f)
	}
	if f.Range != "7d" || f.Page != 1 {
		t.Fatalf("defaults not applied: %+v", f)
	}
	if got := f.filterQuery(); got != "event_type: llm_call" {
		t.Fatalf("query = %q", got)
	}
}

func TestParseLogFilterAcceptsGoodValues(t *testing.T) {
	f := parseLogFilter(url.Values{
		// The console links people by Windows user; the gateway writes
		// "emp-peter". Both spellings have to land on the same filter.
		"employee": {"EMP-Peter"},
		"status":   {"cancelled"},
		"model":    {"deepseek-v3.2"},
		"range":    {"custom"},
		"from":     {"2026-09-01"},
		"to":       {"2026-09-03"},
		"page":     {"4"},
	})
	want := logFilter{Employee: "peter", Status: "cancelled", Model: "deepseek-v3.2",
		Range: "custom", From: "2026-09-01", To: "2026-09-03", Page: 4}
	if f != want {
		t.Fatalf("\n got %+v\nwant %+v", f, want)
	}
	if got := parseLogFilter(url.Values{"page": {"9999"}}).Page; got != logsMaxPage {
		t.Errorf("page clamp = %d, want %d", got, logsMaxPage)
	}
}

func TestResolveWindow(t *testing.T) {
	loc := shanghai()
	now := time.Date(2026, 9, 12, 15, 30, 0, 0, loc)

	f := logFilter{Range: "7d"}
	from, to := f.resolveWindow(now)
	if to != now.Unix() {
		t.Errorf("7d: to = %d, want %d", to, now.Unix())
	}
	if want := now.AddDate(0, 0, -7).Unix(); from != want {
		t.Errorf("7d: from = %d, want %d", from, want)
	}

	f = logFilter{Range: "today"}
	from, _ = f.resolveWindow(now)
	if want := time.Date(2026, 9, 12, 0, 0, 0, 0, loc).Unix(); from != want {
		t.Errorf("today: from = %d, want midnight %d", from, want)
	}

	// A custom range covers whole days in Asia/Shanghai, end included.
	f = logFilter{Range: "custom", From: "2026-09-01", To: "2026-09-01"}
	from, to = f.resolveWindow(now)
	if want := time.Date(2026, 9, 1, 0, 0, 0, 0, loc).Unix(); from != want {
		t.Errorf("custom: from = %d, want %d", from, want)
	}
	if want := time.Date(2026, 9, 1, 23, 59, 59, 0, loc).Unix(); to != want {
		t.Errorf("custom: to = %d, want %d", to, want)
	}

	// Backwards, too wide, or half-filled all fall back to the default -- and
	// the filter is rewritten, so the form redraws as what was actually run.
	for _, bad := range []logFilter{
		{Range: "custom", From: "2026-09-10", To: "2026-09-01"},
		{Range: "custom", From: "2026-01-01", To: "2026-09-01"},
		{Range: "custom", From: "2026-09-01"},
	} {
		f := bad
		from, to := f.resolveWindow(now)
		if f.Range != "7d" || f.From != "" || f.To != "" {
			t.Errorf("%+v was not reset: %+v", bad, f)
		}
		if want := now.AddDate(0, 0, -7).Unix(); from != want || to != now.Unix() {
			t.Errorf("%+v did not fall back to 7 days: %d..%d", bad, from, to)
		}
	}
}

func TestPageLinksCarryTheFilter(t *testing.T) {
	f := logFilter{Employee: "peter", Status: "failure", Model: "glm-5", Range: "30d", Page: 2}

	prev, next := f.pageLinks(logsPageSize)
	for _, href := range []string{prev, next} {
		for _, want := range []string{"employee=peter", "status=failure", "model=glm-5", "range=30d"} {
			if !strings.Contains(href, want) {
				t.Errorf("%q lost %q", href, want)
			}
		}
	}
	// Page 1 is the bare URL, not "page=1".
	if strings.Contains(prev, "page=") {
		t.Errorf("prev = %q, want no page parameter", prev)
	}
	if !strings.Contains(next, "page=3") {
		t.Errorf("next = %q", next)
	}

	// A short page is the last page.
	if _, next := f.pageLinks(3); next != "" {
		t.Errorf("a short page offered a next link: %q", next)
	}
	if prev, _ := (logFilter{Range: "7d", Page: 1}).pageLinks(logsPageSize); prev != "" {
		t.Errorf("page 1 offered a previous link: %q", prev)
	}
	// The cap is a real stop, not just a clamp on the way in.
	if _, next := (logFilter{Range: "7d", Page: logsMaxPage}).pageLinks(logsPageSize); next != "" {
		t.Errorf("the last page offered a next link: %q", next)
	}
}

func TestProbeHealthStates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		oks, fails  string
		wantSev     string
		wantLabel   string
		wantsDetail bool
	}{
		{"all good", "58", "0", "ok", "网关正常", false},
		{"some failures", "40", "2", "bad", "网关有失败", false},
		{"nothing at all", "0", "0", "warn", "无探测数据", true},
	} {
		p := probeFrom([]slsclient.Log{{"oks": tc.oks, "fails": tc.fails}}, nil)
		if p.Sev != tc.wantSev || p.Label != tc.wantLabel {
			t.Errorf("%s: sev=%q label=%q", tc.name, p.Sev, p.Label)
		}
		if (p.Detail != "") != tc.wantsDetail {
			t.Errorf("%s: detail = %q", tc.name, p.Detail)
		}
	}

	// A failure gets a time and a message, so the bar says what broke.
	p := probeFrom(
		[]slsclient.Log{{"oks": "40", "fails": "2"}},
		[]slsclient.Log{{"occurred_at": "2026-09-12T02:41:08.220Z", "message": "probe failed: 502"}},
	)
	if p.LastFailAt == "" || p.LastFailMsg != "probe failed: 502" {
		t.Fatalf("last failure not carried: %+v", p)
	}
}

// An unpriced call is not a free one. Showing "0.000000" for a call the
// gateway could not price would understate somebody's spend.
func TestRowFormatting(t *testing.T) {
	rows := logRowsFrom([]slsclient.Log{
		{"occurred_at": "2026-09-12T03:12:44.118Z", "employee_id": "emp-peter", "status": "success",
			"latency_ms": "1483.7", "total_tokens": "1520", "cost_usd": "0.000421", "cost_state": "estimated"},
		{"employee_id": "console@host", "status": "failure", "cost_state": "unknown",
			"error_class": "rate_limit", "error_code": "429"},
	})
	if rows[0].Employee != "peter" || rows[0].Latency != "1484" || rows[0].Cost != "0.000421" {
		t.Fatalf("row 0 = %+v", rows[0])
	}
	if rows[0].Time != "09-12 11:12:44" { // Asia/Shanghai
		t.Errorf("row 0 time = %q", rows[0].Time)
	}
	if rows[1].Cost != "未知" || rows[1].Error != "rate_limit 429" || rows[1].Tokens != em {
		t.Fatalf("row 1 = %+v", rows[1])
	}
	// An id in another shape is shown but not linked to an account that does
	// not exist.
	if rows[1].Employee != "" || rows[1].EmployeeID != "console@host" {
		t.Errorf("row 1 employee = %+v", rows[1])
	}
}

// ---- handler-level, against a stand-in SLS ----

// slsStub records what the console asked and answers with canned rows.
type slsStub struct {
	srv     *httptest.Server
	mu      chan struct{} // a lock cheap enough not to need sync in a test
	queries []string
	stores  []string

	status    int
	errorCode string
	progress  string
	// progressFor overrides progress for one logstore, so a probe query can
	// be behind while the table's is not.
	progressFor map[string]string
	rows        map[string]string // logstore+"|agg" or logstore+"|list" -> JSON array
}

func newSLSStub(t *testing.T) *slsStub {
	t.Helper()
	st := &slsStub{
		mu: make(chan struct{}, 1), status: http.StatusOK, progress: "Complete",
		errorCode:   "Unauthorized",
		progressFor: map[string]string{},
		rows:        map[string]string{},
	}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store := strings.TrimPrefix(r.URL.Path, "/logstores/")
		q := r.URL.Query().Get("query")
		st.mu <- struct{}{}
		st.queries = append(st.queries, q)
		st.stores = append(st.stores, store)
		<-st.mu

		if st.status != http.StatusOK {
			w.WriteHeader(st.status)
			w.Write([]byte(`{"errorCode":"` + st.errorCode + `","errorMessage":"denied"}`))
			return
		}
		kind := "list"
		if strings.Contains(q, "|") {
			kind = "agg"
		}
		body := st.rows[store+"|"+kind]
		if body == "" {
			body = "[]"
		}
		progress := st.progress
		if p, ok := st.progressFor[store]; ok {
			progress = p
		}
		w.Header().Set("x-log-progress", progress)
		w.Write([]byte(body))
	}))
	t.Cleanup(st.srv.Close)
	return st
}

func (st *slsStub) asked(want string) bool {
	for _, q := range st.queries {
		if q == want {
			return true
		}
	}
	return false
}

// logsServer is a console with the log page enabled, pointed at the stub.
func logsServer(t *testing.T, st *slsStub) (*Server, *http.Cookie) {
	t.Helper()
	s, err := New(Options{Listen: "127.0.0.1:0", SLSProject: "windows-control-logs", SLSEndpoint: st.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	s.dialOSS = func(config.Config) (store, error) { return fs, nil }
	return s, signIn(t, s)
}

func getLogs(t *testing.T, s *Server, cookie *http.Cookie, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/logs"+query, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestLogsPageQueriesAndRenders(t *testing.T) {
	st := newSLSStub(t)
	st.rows["audit|agg"] = `[{"calls":"12","failures":"1","cancelled":"0","cost":"0.4213"}]`
	st.rows["audit|list"] = `[{"occurred_at":"2026-09-12T03:12:44.118Z","employee_id":"emp-peter",` +
		`"status":"success","model_group":"glm-5","model":"zhipu/glm-5","latency_ms":"1483",` +
		`"total_tokens":"1520","cost_usd":"0.000421","cost_state":"estimated"}]`
	st.rows["ops|agg"] = `[{"oks":"58","fails":"0"}]`

	s, cookie := logsServer(t, st)
	rec := getLogs(t, s, cookie, "?employee=peter&status=success")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"网关正常", "调用 <b class=\"mono\">12</b>", "$0.4213",
		`<a href="/users/detail?user=peter">peter</a>`, "glm-5", "0.000421",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}

	// Both audit queries carry the filter, and the probe asks ops separately.
	if !st.asked(`event_type: llm_call and employee_id: "emp-peter" and status: success`) {
		t.Errorf("list query not sent: %q", st.queries)
	}
	if !st.asked(probeSummaryQuery) {
		t.Errorf("probe query not sent: %q", st.queries)
	}
	var sawOps bool
	for _, store := range st.stores {
		if store == logstoreOps {
			sawOps = true
		}
	}
	if !sawOps {
		t.Errorf("the probe bar did not read the ops logstore: %q", st.stores)
	}
}

// A key that cannot read the logs is a deployment problem with a known fix.
// The page has to say so and still render -- an error page would leave an
// operator with nothing but a status code.
func TestLogsPageExplainsAForbiddenKey(t *testing.T) {
	st := newSLSStub(t)
	st.status = http.StatusForbidden

	s, cookie := logsServer(t, st)
	rec := getLogs(t, s, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "AliyunLogReadOnlyAccess") {
		t.Fatal("the page does not name the policy to add")
	}
	// The form is still there, so the page is usable once the key is fixed.
	if !strings.Contains(body, `<form method="get" action="/logs"`) {
		t.Error("the filter form was dropped")
	}
	// And nothing claims there were no calls: the console has no idea.
	if strings.Contains(body, "这段时间没有调用记录") {
		t.Error("a failed query was rendered as an empty week")
	}
}

// A signing bug and a missing policy both arrive as 401/403, and no RAM
// policy will fix the first. The errorCode is the only thing that tells them
// apart, so it has to reach the page.
func TestLogsPageNamesTheErrorCodeBehindAForbidden(t *testing.T) {
	st := newSLSStub(t)
	st.status = http.StatusUnauthorized
	st.errorCode = "SignatureNotMatch"

	s, cookie := logsServer(t, st)
	body := getLogs(t, s, cookie, "").Body.String()
	if !strings.Contains(body, "SignatureNotMatch") {
		t.Fatal("the page hid the errorCode behind the permission hint")
	}
}

// The health bar counts over an hour of probes. An index that is still
// catching up under-counts, and under-counting failures turns a red bar
// green -- the page has to say the tally is provisional.
func TestLogsPageWarnsWhenTheProbeIndexIsBehind(t *testing.T) {
	st := newSLSStub(t)
	st.progressFor[logstoreOps] = "Incomplete"
	st.rows["ops|agg"] = `[{"oks":"58","fails":"0"}]`

	s, cookie := logsServer(t, st)
	body := getLogs(t, s, cookie, "").Body.String()
	if !strings.Contains(body, "结果可能不完整") {
		t.Fatal("a partial probe tally was presented as the hour's total")
	}
}

func TestLogsPageWarnsWhenTheIndexIsBehind(t *testing.T) {
	st := newSLSStub(t)
	st.progress = "Incomplete"
	st.rows["ops|agg"] = `[{"oks":"0","fails":"0"}]`

	s, cookie := logsServer(t, st)
	body := getLogs(t, s, cookie, "").Body.String()
	if !strings.Contains(body, "结果可能不完整") {
		t.Error("an incomplete result was presented as a whole one")
	}
	// And with no probes at all, the bar says nobody is watching rather than
	// showing green.
	if !strings.Contains(body, "无探测数据") {
		t.Error("an empty probe window did not read as a warning")
	}
}

func TestLogsPagePagination(t *testing.T) {
	st := newSLSStub(t)
	rows := make([]string, logsPageSize)
	for i := range rows {
		rows[i] = `{"event_id":"e","status":"success","employee_id":"emp-peter"}`
	}
	st.rows["audit|list"] = "[" + strings.Join(rows, ",") + "]"

	s, cookie := logsServer(t, st)
	body := getLogs(t, s, cookie, "?employee=peter&page=2").Body.String()
	if !strings.Contains(body, "/logs?employee=peter&amp;page=3&amp;range=7d") {
		t.Error("no next-page link carrying the filter")
	}
	if !strings.Contains(body, `href="/logs?employee=peter&amp;range=7d"`) {
		t.Error("no previous-page link back to page 1")
	}
}

func TestLogsPageEmptyResult(t *testing.T) {
	st := newSLSStub(t)
	s, cookie := logsServer(t, st)
	body := getLogs(t, s, cookie, "").Body.String()
	if !strings.Contains(body, "这段时间没有调用记录") {
		t.Error("an empty result did not say so")
	}
}

// Without a project there is no page at all: the nav does not offer it and
// the route does not exist, so a stale bookmark 404s rather than rendering
// controls that cannot work.
func TestLogsPageAbsentWithoutAProject(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	cookie := signIn(t, s)

	if rec := getLogs(t, s, cookie, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/overview", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `href="/logs"`) {
		t.Error("the nav offers a page that 404s")
	}
}

// The session holds the SLS client; nothing about it may reach the browser.
func TestLogsPageLeaksNoCredential(t *testing.T) {
	st := newSLSStub(t)
	st.rows["ops|agg"] = `[{"oks":"1","fails":"0"}]`
	s, cookie := logsServer(t, st)
	body := getLogs(t, s, cookie, "").Body.String()
	for _, secret := range []string{testAccessKeyID, testAccessKeySecret} {
		if strings.Contains(body, secret) {
			t.Fatalf("the page contains %q", secret)
		}
	}
	sess := s.currentSession(&http.Request{Header: http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}}})
	if sess == nil || sess.sls == nil {
		t.Fatal("sign-in did not build an SLS client")
	}
	if sess.sls.Project() != "windows-control-logs" {
		t.Errorf("project = %q", sess.sls.Project())
	}
}
