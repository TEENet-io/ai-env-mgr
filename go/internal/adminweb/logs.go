package adminweb

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/slsclient"
)

// The two logstores this page reads. Both are deployment constants: the
// project is named by --sls-project, but a console that pointed these
// elsewhere would simply find nothing.
const (
	logstoreAudit = "audit"
	logstoreOps   = "ops"
)

const (
	// logsPageSize is both the page size and the "is there a next page" test:
	// a short page is the last one. SLS offers no total, and asking for one
	// would mean a second aggregate query per page view.
	logsPageSize = 100

	// logsMaxPage bounds the offset a URL can ask for. Deep pagination over
	// SLS gets slower the further out it goes, and nobody reads page 51 of a
	// call log -- they narrow the filter instead.
	logsMaxPage = 50

	// logsMaxSpanDays caps a custom range. Wider than this is a data export,
	// not a console page, and it makes SLS scan far more than a browser
	// request should wait for.
	logsMaxSpanDays = 90

	// slsQueryTimeout bounds one query; logsPageBudget bounds the whole page,
	// so a string of slow queries cannot add up past what a browser will wait.
	slsQueryTimeout = 15 * time.Second
	logsPageBudget  = 30 * time.Second

	// probeWindow is how far back the health bar looks. The prober runs every
	// few minutes, so an hour is several samples -- enough that one missed
	// scrape does not read as an outage.
	probeWindow = 60 * time.Minute
)

// Input is validated against allow-lists, never escaped.
//
// Everything here is concatenated into an SLS query expression, so the
// question is not "can this be escaped" but "can this contain anything that
// needs escaping". These patterns say no: no quotes, no spaces, no backslash,
// no query operators. A value that does not match is dropped and the field
// falls back to its default -- silently, because a typo in a URL is not worth
// an error page.
var (
	reEmployee = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)
	reModel    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	reDate     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// shanghai is the zone this console reads and writes wall-clock time in.
//
// Everything in SLS is UTC; everyone reading this page is in China. The
// FixedZone fallback is for a Windows host with no tzdata, where
// LoadLocation fails and the alternative would be silently showing UTC.
var shanghai = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*60*60)
})

// logFilter is the page's form, after validation. Every field is either a
// value that passed its allow-list or the default.
type logFilter struct {
	Employee string // windows user, lowercase, without the gateway's "emp-" prefix
	Status   string // "", success, failure, cancelled
	Model    string // a model_group
	Range    string // today, 7d, 30d, custom
	From     string // YYYY-MM-DD, custom only
	To       string
	Page     int
}

var logStatuses = []struct{ Value, Label string }{
	{"", "全部"},
	{"success", "成功"},
	{"failure", "失败"},
	{"cancelled", "取消"},
}

var logRanges = []struct{ Value, Label string }{
	{"today", "今天"},
	{"7d", "最近 7 天"},
	{"30d", "最近 30 天"},
	{"custom", "自定义"},
}

// parseLogFilter reads the query string. It cannot fail: anything it does not
// recognise becomes the default.
func parseLogFilter(q url.Values) logFilter {
	f := logFilter{Range: "7d", Page: 1}

	// employee_id is spelled two ways in this system. The gateway writes the
	// LiteLLM user id, which admincore.KeyAlias builds as "emp-" + the
	// lowercased Windows user; every link in this console -- /users/detail,
	// the roster, the dropdown below -- names the same person by the bare
	// Windows user. Strip the prefix here so both spellings land on one
	// filter, and put it back in filterQuery, which is the only place that
	// has to match what is actually in the logstore.
	emp := strings.ToLower(strings.TrimSpace(q.Get("employee")))
	emp = strings.TrimPrefix(emp, "emp-")
	if reEmployee.MatchString(emp) {
		f.Employee = emp
	}

	switch st := strings.TrimSpace(q.Get("status")); st {
	case "success", "failure", "cancelled":
		f.Status = st
	}

	if m := strings.TrimSpace(q.Get("model")); reModel.MatchString(m) {
		f.Model = m
	}

	switch rg := strings.TrimSpace(q.Get("range")); rg {
	case "today", "7d", "30d", "custom":
		f.Range = rg
	}
	if f.Range == "custom" {
		if v := strings.TrimSpace(q.Get("from")); reDate.MatchString(v) {
			f.From = v
		}
		if v := strings.TrimSpace(q.Get("to")); reDate.MatchString(v) {
			f.To = v
		}
	}

	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("page"))); err == nil && n >= 1 {
		f.Page = min(n, logsMaxPage)
	}
	return f
}

// resolveWindow turns the range into [from, to] in unix seconds, rewriting
// the filter when a custom range does not hold up -- a missing end, a backwards
// pair, or a span past the cap all fall back to the default, so the form that
// is drawn afterwards shows what was actually queried rather than what was
// asked for.
func (f *logFilter) resolveWindow(now time.Time) (int64, int64) {
	loc := shanghai()
	now = now.In(loc)

	if f.Range == "custom" {
		from, okF := time.ParseInLocation("2006-01-02", f.From, loc)
		to, okT := time.ParseInLocation("2006-01-02", f.To, loc)
		// The end date is inclusive: somebody asking for 09-01 to 09-01 means
		// that whole day, not its first instant.
		end := to.Add(24*time.Hour - time.Second)
		if okF == nil && okT == nil && !end.Before(from) && end.Sub(from) <= logsMaxSpanDays*24*time.Hour {
			return from.Unix(), end.Unix()
		}
		f.Range, f.From, f.To = "7d", "", ""
	}

	switch f.Range {
	case "today":
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		return start.Unix(), now.Unix()
	case "30d":
		return now.AddDate(0, 0, -30).Unix(), now.Unix()
	default:
		return now.AddDate(0, 0, -7).Unix(), now.Unix()
	}
}

// rangeLabel names the window in the heading, so a screenshot says what it
// covers without the URL.
func (f logFilter) rangeLabel() string {
	switch f.Range {
	case "today":
		return "今天"
	case "30d":
		return "最近 30 天"
	case "custom":
		return f.From + " 至 " + f.To
	default:
		return "最近 7 天"
	}
}

// filterQuery is the SLS search expression for the current filter.
//
// The values are interpolated rather than escaped because they have already
// been through the allow-lists above; nothing that reaches here can contain a
// quote, a space or an operator.
func (f logFilter) filterQuery() string {
	q := "event_type: llm_call"
	if f.Employee != "" {
		q += ` and employee_id: "` + admincore.KeyAlias(f.Employee) + `"`
	}
	if f.Status != "" {
		q += " and status: " + f.Status
	}
	if f.Model != "" {
		q += ` and model_group: "` + f.Model + `"`
	}
	return q
}

// summaryQuery counts the same set the table lists.
//
// The inner "select distinct event_id" is what keeps the totals honest: a
// single call can land in the logstore more than once -- Logtail re-reads a
// file tail after a rotation, and the gateway's own retry writes the same
// event_id again -- and summing cost_usd over the raw rows would bill an
// employee twice for one request.
func (f logFilter) summaryQuery() string {
	return f.filterQuery() +
		` | select count(*) as calls, count_if(status='failure') as failures,` +
		` count_if(status='cancelled') as cancelled,` +
		` round(sum(try_cast(cost_usd as double)),4) as cost` +
		` from (select distinct event_id, status, cost_usd from log)`
}

const (
	probeFilterQuery  = "module: probe and target: gateway"
	probeSummaryQuery = probeFilterQuery +
		" | select count_if(ok='true') as oks, count_if(ok='false') as fails"
	probeFailQuery = probeFilterQuery + " and ok: false"
)

// logRow is one call, already formatted. The template does no arithmetic and
// no parsing: a column that could not be read is an em dash here, not a
// template that silently renders nothing.
type logRow struct {
	Time string
	// Employee is the Windows user behind an "emp-" id. OffRoster says nobody
	// on the roster answers to it any more, which is what decides whether it
	// is drawn as a link: /users/detail for a departed employee is a 404.
	Employee    string
	OffRoster   bool
	Master      bool   // the gateway's own key rather than any employee
	EmployeeID  string // raw employee_id, when it is none of the above
	ModelGroup  string
	Model       string
	StatusLabel string
	StatusSev   string // ok / warn / bad, for the tag colour
	Latency     string
	Tokens      string
	Cost        string
	Error       string
}

type logsSummary struct {
	Calls     string
	Failures  string
	Cancelled string
	Cost      string
}

// probeHealth is the bar at the top: what the gateway prober has seen in the
// last hour.
type probeHealth struct {
	OKs         int
	Fails       int
	Sev         string // ok / warn / bad
	Label       string
	Detail      string
	LastFailAt  string
	LastFailMsg string
}

// logsPage is everything logs.html draws. It is built by pure functions from
// query results, so the preview renderer can fill one in by hand.
type logsPage struct {
	// Project names the SLS project in the subtitle, so a console pointed at
	// a staging project does not claim to be showing production.
	Project    string
	Filter     logFilter
	RangeLabel string
	Employees  []string
	Statuses   []struct{ Value, Label string }
	Ranges     []struct{ Value, Label string }
	Probe      *probeHealth
	Summary    *logsSummary
	Rows       []logRow
	// ListOK says the table's query actually ran. Without it a failed query
	// and a quiet week look identical, and "这段时间没有调用记录" would be a
	// claim the console is in no position to make.
	ListOK     bool
	Incomplete bool
	Notice     string
	PrevURL    string
	NextURL    string
}

// newLogsPage seeds the parts that never depend on SLS, so a page whose
// queries all failed still draws its form.
func newLogsPage(project string, f logFilter, employees []string) *logsPage {
	return &logsPage{
		Project:    project,
		Filter:     f,
		RangeLabel: f.rangeLabel(),
		Employees:  employees,
		Statuses:   logStatuses,
		Ranges:     logRanges,
	}
}

// em is what an absent value looks like. SLS omits optional fields entirely
// rather than writing an empty string, so most rows are missing something.
const em = "—"

func field(l slsclient.Log, key string) string { return strings.TrimSpace(l[key]) }

// fieldOr returns a field or the em dash.
func fieldOr(l slsclient.Log, key string) string {
	if v := field(l, key); v != "" {
		return v
	}
	return em
}

// masterKeyID is what LiteLLM records for a call made with the master key
// rather than with an employee's own. It is not a user id and there is no
// account page behind it, so the table names it for what it is.
const masterKeyID = "default_user_id"

// logRowsFrom formats the table. The roster decides which employee ids are
// still links; it is the one the handler already loaded for the filter
// dropdown, so this costs no extra read.
//
// A nil roster means it could not be read. That is deliberately not the same
// as an empty one: marking every row 不在名册 because OSS hiccuped would be
// the console asserting something it does not know.
func logRowsFrom(logs []slsclient.Log, roster *model.Users) []logRow {
	rows := make([]logRow, 0, len(logs))
	for _, l := range logs {
		r := logRow{
			Time:       displayTime(l),
			ModelGroup: fieldOr(l, "model_group"),
			Model:      field(l, "model"),
			Latency:    intish(field(l, "latency_ms")),
			Tokens:     fieldOr(l, "total_tokens"),
			Cost:       costOf(l),
			Error:      errorOf(l),
		}
		id := field(l, "employee_id")
		user, isEmp := strings.CutPrefix(id, "emp-")
		switch {
		case isEmp && user != "":
			r.Employee = user
			r.OffRoster = roster != nil && roster.Find(user) == nil
		case id == masterKeyID:
			r.Master = true
		case id != "":
			// Something wrote an employee_id in another shape. Show it, but do
			// not link it to an account page that would 404.
			r.EmployeeID = id
		}
		r.StatusLabel, r.StatusSev = statusLabel(field(l, "status"))
		rows = append(rows, r)
	}
	return rows
}

// displayTime prefers the event's own stamp over the one the collector gave
// it: occurred_at is when the call happened, __time__ is when SLS heard about
// it, and after a backlog is replayed those differ by hours.
func displayTime(l slsclient.Log) string {
	if v := field(l, "occurred_at"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.In(shanghai()).Format("01-02 15:04:05")
		}
		return v
	}
	if v := field(l, "__time__"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(secs, 0).In(shanghai()).Format("01-02 15:04:05")
		}
	}
	return em
}

func statusLabel(status string) (string, string) {
	switch status {
	case "success":
		return "成功", "ok"
	case "failure":
		return "失败", "bad"
	case "cancelled":
		return "取消", "warn"
	case "":
		return em, "warn"
	default:
		return status, "warn"
	}
}

// intish rounds a numeric field to whole units. Latencies arrive as floats
// with more precision than anyone reads.
func intish(v string) string {
	if v == "" {
		return em
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return v
	}
	return strconv.FormatFloat(f, 'f', 0, 64)
}

// costOf renders the cost. cost_state=unknown means the gateway could not
// price the call at all, which is a different thing from "$0.000000" and has
// to read differently or a free-looking row hides an unmetered one.
func costOf(l slsclient.Log) string {
	if field(l, "cost_state") == "unknown" {
		return "未知"
	}
	v := field(l, "cost_usd")
	if v == "" {
		return em
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return v
	}
	return strconv.FormatFloat(f, 'f', 6, 64)
}

func errorOf(l slsclient.Log) string {
	class, code := field(l, "error_class"), field(l, "error_code")
	switch {
	case class != "" && code != "":
		return class + " " + code
	case class != "":
		return class
	case code != "":
		return code
	default:
		return em
	}
}

// summaryFrom reads the aggregate query's single row.
func summaryFrom(logs []slsclient.Log) *logsSummary {
	s := &logsSummary{Calls: "0", Failures: "0", Cancelled: "0", Cost: "0.0000"}
	if len(logs) == 0 {
		return s
	}
	l := logs[0]
	if v := field(l, "calls"); v != "" {
		s.Calls = v
	}
	if v := field(l, "failures"); v != "" {
		s.Failures = v
	}
	if v := field(l, "cancelled"); v != "" {
		s.Cancelled = v
	}
	// sum() over no rows is null, which arrives as "" or "null".
	if v := field(l, "cost"); v != "" && v != "null" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.Cost = strconv.FormatFloat(f, 'f', 4, 64)
		} else {
			s.Cost = v
		}
	}
	return s
}

// probeFrom builds the health bar from the aggregate row and, when there were
// failures, the most recent failing probe.
//
// Three states, and the third one matters most: no probes at all is not
// "healthy", it is "nobody is watching", which is exactly the failure a green
// bar would hide.
func probeFrom(summary []slsclient.Log, lastFail []slsclient.Log) *probeHealth {
	p := &probeHealth{}
	if len(summary) > 0 {
		p.OKs = atoiOr(field(summary[0], "oks"), 0)
		p.Fails = atoiOr(field(summary[0], "fails"), 0)
	}
	switch {
	case p.Fails > 0:
		p.Sev, p.Label = "bad", "网关有失败"
	case p.OKs > 0:
		p.Sev, p.Label = "ok", "网关正常"
	default:
		p.Sev, p.Label = "warn", "无探测数据"
		p.Detail = "探测器或采集可能停了。"
	}
	if len(lastFail) > 0 {
		p.LastFailAt = displayTime(lastFail[0])
		p.LastFailMsg = field(lastFail[0], "message")
	}
	return p
}

func atoiOr(v string, def int) int {
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	// An aggregate can come back as "3.0" on some paths; do not lose it.
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return int(f)
	}
	return def
}

// pageLinks builds the previous/next hrefs, carrying every filter along. A
// short page is the last page: SLS does not report a total and asking for one
// would cost another query on every view.
func (f logFilter) pageLinks(rowsOnPage int) (string, string) {
	href := func(page int) string {
		v := url.Values{}
		if f.Employee != "" {
			v.Set("employee", f.Employee)
		}
		if f.Status != "" {
			v.Set("status", f.Status)
		}
		if f.Model != "" {
			v.Set("model", f.Model)
		}
		v.Set("range", f.Range)
		if f.Range == "custom" {
			v.Set("from", f.From)
			v.Set("to", f.To)
		}
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		return "/logs?" + v.Encode()
	}
	var prev, next string
	if f.Page > 1 {
		prev = href(f.Page - 1)
	}
	if rowsOnPage >= logsPageSize && f.Page < logsMaxPage {
		next = href(f.Page + 1)
	}
	return prev, next
}

// slsNotice turns a query failure into something an operator can act on.
// A permission problem names the policy to attach; anything else shows the
// errorCode, which is what an Alibaba Cloud ticket will ask for.
func slsNotice(err error) string {
	if errors.Is(err, slsclient.ErrForbidden) {
		hint := "这把 AccessKey 没有日志读取权限，请给它添加 AliyunLogReadOnlyAccess"
		// The errorCode distinguishes the two things a 401/403 can mean. A
		// missing policy says Unauthorized; a signing bug says
		// SignatureNotMatch, and no amount of RAM policy will fix that one.
		if code := slsclient.Code(err); code != "" {
			hint += "（" + code + "）"
		}
		return hint
	}
	return "查询 SLS 失败：" + err.Error()
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, sess *session) {
	// Not configured means the page does not exist, rather than a page that
	// explains it cannot work: the nav does not offer it either.
	if s.opts.SLSProject == "" {
		http.NotFound(w, r)
		return
	}
	data := newPage(sess, r, "logs")

	f := parseLogFilter(r.URL.Query())
	from, to := f.resolveWindow(time.Now())
	// Read once and used twice: the filter dropdown lists it, and the table
	// asks it whether each employee id still belongs to somebody.
	roster := loadRoster(sess)
	page := newLogsPage(s.opts.SLSProject, f, employeeIDs(roster))
	data.Logs = page

	s.events.Ops("info", "admin_query", "logs query", map[string]any{
		"employee": f.Employee, "status": f.Status, "range": f.Range, "page": f.Page,
	})

	if sess.sls == nil {
		page.Notice = "本次登录没有建立 SLS 客户端，请重新登录。"
		s.render(w, "logs.html", http.StatusOK, data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), logsPageBudget)
	defer cancel()

	// One query per section, in series. Concurrency would save a second at
	// most and would make a single slow query harder to attribute; a failure
	// costs its own section and nothing else.
	query := func(logstore, q string, line, offset int, reverse bool) *slsclient.Result {
		qctx, qcancel := context.WithTimeout(ctx, slsQueryTimeout)
		defer qcancel()
		res, err := sess.sls.GetLogs(qctx, logstore, q, from, to, line, offset, reverse)
		if err != nil {
			if page.Notice == "" {
				page.Notice = slsNotice(err)
			}
			// The message is safe to log -- it carries an errorCode, never the
			// key that failed to authenticate.
			log.Printf("adminweb: sls %s: %v", logstore, err)
			return nil
		}
		if !res.Complete {
			page.Incomplete = true
		}
		return res
	}

	if res := query(logstoreAudit, f.summaryQuery(), 1, 0, false); res != nil {
		page.Summary = summaryFrom(res.Logs)
	}
	if res := query(logstoreAudit, f.filterQuery(), logsPageSize, (f.Page-1)*logsPageSize, true); res != nil {
		page.ListOK = true
		page.Rows = logRowsFrom(res.Logs, roster)
		page.PrevURL, page.NextURL = f.pageLinks(len(res.Logs))
	}
	s.loadProbe(ctx, sess, page)

	s.render(w, "logs.html", http.StatusOK, data)
}

// loadProbe fills the health bar. Its window is the last hour regardless of
// what the table is showing: "is the gateway up right now" is not a question
// about the range being browsed.
func (s *Server) loadProbe(ctx context.Context, sess *session, page *logsPage) {
	now := time.Now()
	from, to := now.Add(-probeWindow).Unix(), now.Unix()

	// The bool is "the query ran", which is not the same as "it matched
	// something": an aggregate over an empty hour answers with an empty array,
	// and that is the answer the bar most needs to report.
	run := func(q string, line int, reverse bool) ([]slsclient.Log, bool) {
		qctx, cancel := context.WithTimeout(ctx, slsQueryTimeout)
		defer cancel()
		res, err := sess.sls.GetLogs(qctx, logstoreOps, q, from, to, line, 0, reverse)
		if err != nil {
			if page.Notice == "" {
				page.Notice = slsNotice(err)
			}
			log.Printf("adminweb: sls %s (probe): %v", logstoreOps, err)
			return nil, false
		}
		// An index still catching up under-counts, and under-counting here
		// turns a red bar green. Say so with the same warning the table uses
		// rather than presenting a partial tally as the hour's total.
		if !res.Complete {
			page.Incomplete = true
		}
		return res.Logs, true
	}

	summary, ok := run(probeSummaryQuery, 1, false)
	if !ok {
		return
	}
	var lastFail []slsclient.Log
	// Only worth a second query when there is a failure to describe.
	if len(summary) > 0 && atoiOr(field(summary[0], "fails"), 0) > 0 {
		lastFail, _ = run(probeFailQuery, 1, true)
	}
	page.Probe = probeFrom(summary, lastFail)
}

// loadRoster reads the employee roster, or nil if it could not be read. The
// log page is still worth drawing without it: the dropdown goes empty and no
// employee id is judged against a roster nobody has seen.
func loadRoster(sess *session) *model.Users {
	users, err := sess.be.Roster(context.Background())
	if err != nil {
		log.Printf("adminweb: LoadUsers for the log page: %v", err)
		return nil
	}
	return &model.Users{Users: users}
}

// employeeIDs is the dropdown: everyone on the roster, so filtering does not
// require knowing how an id is spelled. Sorted.
func employeeIDs(us *model.Users) []string {
	if us == nil {
		return nil
	}
	ids := make([]string, 0, len(us.Users))
	for _, u := range us.Users {
		if id := strings.ToLower(strings.TrimSpace(u.WindowsUser)); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
