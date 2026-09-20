package adminweb

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// auditPageSize is how many events one page shows; csvLimit bounds an export.
const (
	auditPageSize = 100
	csvLimit      = 50000
)

// auditRow is one event as the page shows it: ids resolved to names, the
// change reduced to what differed.
type auditRow struct {
	repo.AuditEvent
	Target string // the employee's Windows user, the machine's hostname, or the id
	Change string
}

// auditPage is the searchable audit view.
type auditPage struct {
	Filter     repo.AuditFilter
	FromText   string
	ToText     string
	Rows       []auditRow
	Total      int
	Page       int
	Pages      int
	Query      string // the filter as a query string, for page links and the CSV
	Actions    []string
	TargetType string
}

// auditFilterFrom reads the page's query parameters.
func auditFilterFrom(q url.Values) (repo.AuditFilter, string, string, int) {
	f := repo.AuditFilter{
		TargetType: strings.TrimSpace(q.Get("target_type")),
		TargetID:   strings.TrimSpace(q.Get("target")),
		Action:     strings.TrimSpace(q.Get("action")),
		ActorID:    strings.TrimSpace(q.Get("actor")),
	}
	fromText, toText := strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to"))
	if t, err := time.ParseInLocation("2006-01-02", fromText, time.Local); err == nil {
		f.From = t
	} else {
		fromText = ""
	}
	if t, err := time.ParseInLocation("2006-01-02", toText, time.Local); err == nil {
		f.To = t.Add(24 * time.Hour) // inclusive of the day named
	} else {
		toText = ""
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	return f, fromText, toText, page
}

func auditQuery(f repo.AuditFilter, fromText, toText string) string {
	v := url.Values{}
	for k, val := range map[string]string{"target_type": f.TargetType, "target": f.TargetID,
		"action": f.Action, "actor": f.ActorID, "from": fromText, "to": toText} {
		if val != "" {
			v.Set(k, val)
		}
	}
	return v.Encode()
}

// summariseChange names the fields that differ between before and after,
// so a row says what changed rather than that something did. Both sides are
// already redacted upstream.
func summariseChange(before, after []byte) string {
	var a, b map[string]any
	json.Unmarshal(before, &a)
	json.Unmarshal(after, &b)
	if a == nil && b == nil {
		return ""
	}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var changed []string
	for k := range keys {
		switch strings.ToLower(k) {
		case "updatedat", "createdat", "version", "id":
			continue // bookkeeping, not a decision anybody made
		}
		left, _ := json.Marshal(a[k])
		right, _ := json.Marshal(b[k])
		if string(left) == string(right) {
			continue
		}
		switch {
		case a == nil || a[k] == nil:
			changed = append(changed, fmt.Sprintf("%s: %s", k, compactJSON(right)))
		case b == nil || b[k] == nil:
			changed = append(changed, fmt.Sprintf("%s: %s → —", k, compactJSON(left)))
		default:
			changed = append(changed, fmt.Sprintf("%s: %s → %s", k, compactJSON(left), compactJSON(right)))
		}
	}
	sort.Strings(changed)
	return strings.Join(changed, "; ")
}

func compactJSON(b []byte) string {
	s := strings.Trim(string(b), `"`)
	if len(s) > 60 {
		s = s[:57] + "…"
	}
	return s
}

// names resolves target ids to what a person calls them.
func (s *Server) names(r *http.Request) (employees, devices map[string]string) {
	ctx := r.Context()
	employees, devices = map[string]string{}, map[string]string{}
	if list, err := s.dbm.store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true, IncludeDeleted: true}); err == nil {
		for _, e := range list {
			employees[e.ID] = e.WindowsUser
		}
	}
	if list, err := s.dbm.store.Devices().List(ctx, repo.DeviceFilter{IncludeRevoked: true}); err == nil {
		for _, d := range list {
			devices[d.ID] = d.Hostname
		}
	}
	return employees, devices
}

func (s *Server) auditRows(r *http.Request, events []repo.AuditEvent) []auditRow {
	employees, devices := s.names(r)
	rows := make([]auditRow, 0, len(events))
	for _, ev := range events {
		target := ev.TargetID
		switch ev.TargetType {
		case "employee":
			if name, ok := employees[ev.TargetID]; ok {
				target = name
			}
		case "device":
			if name, ok := devices[ev.TargetID]; ok {
				target = name
			}
		}
		rows = append(rows, auditRow{AuditEvent: ev, Target: target, Change: summariseChange(ev.Before, ev.After)})
	}
	return rows
}

// knownActions is what the action filter offers: every action the console
// writes, in the order the file that defines them lists them.
var knownActions = []string{
	"account.onboard", "account.reopen", "account.offboard", "account.reissue", "account.quota",
	"account.models", "account.profile", "account.delete",
	"machine.bind", "machine.unbind", "machine.codex_restart", "machine.sync",
	"policy.publish",
	"release.artifact_register", "release.artifact_status", "release.global_target",
	"release.rollout_create", "release.rollout_pause", "release.rollout_cancel",
	"release.target_exclude", "release.target_retry",
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "audit")
	f, fromText, toText, page := auditFilterFrom(r.URL.Query())
	f.Limit, f.Offset = auditPageSize, (page-1)*auditPageSize
	events, total, err := s.dbm.store.Audit().Search(r.Context(), f)
	if err != nil {
		data.Error = "could not search the audit trail"
	}
	pages := (total + auditPageSize - 1) / auditPageSize
	data.AuditPage = &auditPage{
		Filter: f, FromText: fromText, ToText: toText, Rows: s.auditRows(r, events),
		Total: total, Page: page, Pages: pages, Query: auditQuery(f, fromText, toText), Actions: knownActions,
	}
	s.render(w, "audit.html", http.StatusOK, data)
}

// handleAuditCSV streams the same search as a file. Excel reads a UTF-8 BOM
// as "this is UTF-8", and without it Chinese names come out as noise.
func (s *Server) handleAuditCSV(w http.ResponseWriter, r *http.Request, _ *session) {
	f, fromText, toText, _ := auditFilterFrom(r.URL.Query())
	name := "audit"
	if fromText != "" || toText != "" {
		name += "-" + fromText + "-" + toText
	}
	// The first page is read before any header goes out, so a database that
	// is not answering produces an error page rather than a 200 with an
	// empty file that looks like "no events".
	f.Limit, f.Offset = 1000, 0
	events, _, err := s.dbm.store.Audit().Search(r.Context(), f)
	if err != nil {
		http.Error(w, "导出失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	cw.Write([]string{"occurred_at", "actor_type", "actor", "action", "target_type", "target", "result", "request_id", "change", "before", "after"})
	employees, devices := s.names(r)
	written := 0
	for offset := 0; written < csvLimit; offset += 1000 {
		if offset > 0 {
			f.Offset = offset
			events, _, err = s.dbm.store.Audit().Search(r.Context(), f)
			if err != nil {
				// The status line is long gone; the file itself has to say
				// it is not the whole story.
				cw.Write([]string{"# 导出中断：" + err.Error() + "，以上不是完整结果"})
				break
			}
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			target := ev.TargetID
			if n, ok := employees[ev.TargetID]; ok && ev.TargetType == "employee" {
				target = n
			} else if n, ok := devices[ev.TargetID]; ok && ev.TargetType == "device" {
				target = n
			}
			cw.Write([]string{ev.OccurredAt.Local().Format(time.RFC3339), ev.ActorType, csvSafe(ev.ActorID), csvSafe(ev.Action),
				ev.TargetType, csvSafe(target), csvSafe(ev.Result), csvSafe(ev.RequestID), csvSafe(summariseChange(ev.Before, ev.After)),
				csvSafe(string(ev.Before)), csvSafe(string(ev.After))})
			written++
			if written >= csvLimit {
				cw.Write([]string{"# truncated at " + strconv.Itoa(csvLimit) + " rows"})
				break
			}
		}
		if len(events) < 1000 {
			break
		}
	}
	cw.Flush()
}

// csvSafe keeps a cell from being read as a formula. A spreadsheet treats a
// cell starting with = + - @ (or a tab or return) as something to evaluate,
// and a request id or a name is text an outsider may have chosen. The
// leading apostrophe is the spreadsheet convention for "this is text".
func csvSafe(cell string) string {
	if cell == "" {
		return cell
	}
	switch cell[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + cell
	}
	return cell
}
