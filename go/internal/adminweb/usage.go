package adminweb

import (
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/worker"
)

// usagePage is one month of gateway usage from usage_daily, three ways.
type usagePage struct {
	Month      string // "2026-09"
	Employee   string // Windows user when narrowed to one person
	Months     []string
	ByEmployee []usageLine
	ByModel    []usageLine
	ByDay      []usageLine
	Total      usageLine
	LastRun    string
	Query      string
}

// usageLine is one row of any of the three tables. Pct is the line's share
// of the month's cost, for the bar.
type usageLine struct {
	Label, Sub      string
	Calls, Failures int
	Cost            string
	Unpriced        int
	Tokens          int64
	Pct             int
	Link            string
}

// usageMonth parses month=YYYY-MM, defaulting to the current UTC month, and
// returns its first day and the first day of the next.
func usageMonth(text string, now time.Time) (label string, from, to time.Time) {
	from = time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if t, err := time.Parse("2006-01", strings.TrimSpace(text)); err == nil {
		from = t
	}
	return from.Format("2006-01"), from, from.AddDate(0, 1, 0)
}

// monthsBetween lists the months from last back to first, newest first.
func monthsBetween(first, last time.Time) []string {
	var out []string
	for m := time.Date(last.Year(), last.Month(), 1, 0, 0, 0, 0, time.UTC); !m.Before(time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC)); m = m.AddDate(0, -1, 0) {
		out = append(out, m.Format("2006-01"))
	}
	return out
}

func money4(numeric string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(numeric), 64)
	if err != nil {
		return numeric
	}
	return strconv.FormatFloat(f, 'f', 4, 64)
}

// usageLines turns rows into table lines with names, shares and links.
// label picks the row's identity; total is the month's cost for the share.
func usageLines(rows []repo.UsageRow, total float64, label func(repo.UsageRow) (string, string, string)) []usageLine {
	out := make([]usageLine, 0, len(rows))
	for _, r := range rows {
		name, sub, link := label(r)
		cost, _ := strconv.ParseFloat(r.CostUSD, 64)
		pct := 0
		if total > 0 {
			pct = int(cost / total * 100)
		}
		out = append(out, usageLine{
			Label: name, Sub: sub, Link: link, Calls: r.Calls, Failures: r.Failures,
			Cost: money4(r.CostUSD), Unpriced: r.Unpriced, Tokens: r.PromptTokens + r.CompletionTokens, Pct: pct,
		})
	}
	return out
}

func sumUsage(rows []repo.UsageRow) (usageLine, float64) {
	var t usageLine
	var cost float64
	for _, r := range rows {
		t.Calls += r.Calls
		t.Failures += r.Failures
		t.Unpriced += r.Unpriced
		t.Tokens += r.PromptTokens + r.CompletionTokens
		c, _ := strconv.ParseFloat(r.CostUSD, 64)
		cost += c
	}
	t.Label, t.Cost = "合计", strconv.FormatFloat(cost, 'f', 4, 64)
	return t, cost
}

// employeeNames maps employee ids to Windows user names, deleted ones
// included: last quarter's spend belongs to whoever was there.
func (s *Server) employeeNames(r *http.Request) map[string]repo.Employee {
	names := map[string]repo.Employee{}
	if employees, err := s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true, IncludeDeleted: true}); err == nil {
		for _, e := range employees {
			names[e.ID] = e
		}
	}
	return names
}

func (s *Server) usageFilter(r *http.Request) (repo.UsageFilter, string, string, error) {
	q := r.URL.Query()
	month, from, to := usageMonth(q.Get("month"), time.Now())
	f := repo.UsageFilter{From: from, To: to}
	employee := strings.TrimSpace(q.Get("employee"))
	if employee != "" {
		e, err := s.dbm.store.Employees().ByWindowsUser(r.Context(), employee)
		if err != nil {
			return f, month, employee, err
		}
		f.EmployeeID = e.ID
	}
	return f, month, employee, nil
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "usage")
	f, month, employee, err := s.usageFilter(r)
	if err != nil {
		data.Error = "没有这个员工：" + employee
		employee, f.EmployeeID = "", ""
	}
	page := &usagePage{Month: month, Employee: employee}
	values := url.Values{"month": {month}}
	if employee != "" {
		values.Set("employee", employee)
	}
	page.Query = values.Encode()
	ctx := r.Context()
	store := s.dbm.store.Usage()
	if first, last, err := store.Days(ctx); err == nil {
		page.Months = monthsBetween(first, last)
	} else if !errors.Is(err, repo.ErrNotFound) {
		data.Error = "could not read the usage table"
	}
	if !contains(page.Months, month) {
		page.Months = append([]string{month}, page.Months...)
	}
	names := s.employeeNames(r)
	byEmployee, err1 := store.ByEmployee(ctx, f)
	byModel, err2 := store.ByModel(ctx, f)
	byDay, err3 := store.ByDay(ctx, f)
	if err1 != nil || err2 != nil || err3 != nil {
		data.Error = "could not read the usage table"
	}
	var total float64
	page.Total, total = sumUsage(byEmployee)
	page.ByEmployee = usageLines(byEmployee, total, func(r repo.UsageRow) (string, string, string) {
		if e, ok := names[r.EmployeeID]; ok {
			link := "/usage?" + url.Values{"month": {month}, "employee": {e.WindowsUser}}.Encode()
			return e.WindowsUser, e.Department, link
		}
		return r.KeyAlias, "未关联到员工", ""
	})
	page.ByModel = usageLines(byModel, total, func(r repo.UsageRow) (string, string, string) {
		if r.ModelGroup == "" {
			return "（未记录模型）", "", ""
		}
		return r.ModelGroup, "", ""
	})
	page.ByDay = usageLines(byDay, total, func(r repo.UsageRow) (string, string, string) {
		return r.Day.Format("01-02"), "", ""
	})
	page.LastRun = s.lastRunOf(r, worker.TaskUsageSnapshot)
	data.UsagePage = page
	s.render(w, "usage.html", http.StatusOK, data)
}

// lastRunOf describes the most recent task of a kind, for a page that shows
// data a task keeps fresh.
func (s *Server) lastRunOf(r *http.Request, kind string) string {
	tasks, err := s.dbm.store.Tasks().ListRecent(r.Context(), 100)
	if err != nil {
		return ""
	}
	for _, t := range tasks {
		if t.Kind != kind {
			continue
		}
		at := t.CreatedAt
		if t.FinishedAt != nil {
			at = *t.FinishedAt
		}
		out := at.Local().Format("2006-01-02 15:04") + " " + string(t.Status)
		if t.LastError != "" {
			out += "：" + t.LastError
		}
		return out
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (s *Server) handleUsageCSV(w http.ResponseWriter, r *http.Request, _ *session) {
	f, month, employee, err := s.usageFilter(r)
	if err != nil {
		http.Error(w, "没有这个员工："+employee, http.StatusNotFound)
		return
	}
	rows, err := s.dbm.store.Usage().Rows(r.Context(), f, csvLimit)
	if err != nil {
		http.Error(w, "导出失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	names := s.employeeNames(r)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-`+month+`.csv"`)
	w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	cw.Write([]string{"day", "employee", "department", "key_alias", "model", "calls", "failures", "cost_usd", "unpriced", "prompt_tokens", "completion_tokens"})
	for _, u := range rows {
		name, dept := "", ""
		if e, ok := names[u.EmployeeID]; ok {
			name, dept = e.WindowsUser, e.Department
		}
		cw.Write([]string{u.Day.Format("2006-01-02"), csvSafe(name), csvSafe(dept), csvSafe(u.KeyAlias), csvSafe(u.ModelGroup),
			strconv.Itoa(u.Calls), strconv.Itoa(u.Failures), u.CostUSD, strconv.Itoa(u.Unpriced),
			strconv.FormatInt(u.PromptTokens, 10), strconv.FormatInt(u.CompletionTokens, 10)})
	}
	if len(rows) >= csvLimit {
		cw.Write([]string{"# truncated at " + strconv.Itoa(csvLimit) + " rows"})
	}
	cw.Flush()
}

// actionUsageSnapshotNow queues a snapshot for an administrator who does not
// want to wait for tonight's.
func (s *Server) actionUsageSnapshotNow(_ *session, r *http.Request) (string, error) {
	if err := worker.EnqueueUsageSnapshot(r.Context(), s.dbm.store, time.Now()); err != nil {
		return "", err
	}
	return "已排队，Worker 几秒内开始从日志服务重算最近三天；完成后刷新本页", nil
}

// employeeUsage is the last 30 days for the detail page.
func (s *Server) employeeUsage(r *http.Request, employeeID string) ([]usageLine, usageLine) {
	now := time.Now().UTC().Truncate(24 * time.Hour)
	f := repo.UsageFilter{From: now.AddDate(0, 0, -30), To: now.AddDate(0, 0, 1), EmployeeID: employeeID}
	rows, err := s.dbm.store.Usage().ByDay(r.Context(), f)
	if err != nil {
		return nil, usageLine{}
	}
	total, cost := sumUsage(rows)
	return usageLines(rows, cost, func(r repo.UsageRow) (string, string, string) {
		return r.Day.Format("01-02"), "", ""
	}), total
}
