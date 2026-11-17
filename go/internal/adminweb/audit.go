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
	Actor  string // the person's name where the record keeps only their id
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

// actorNames names the people behind ids. Ticket events record an account's
// id where the rest of the console records its name.
func (s *Server) actorNames(r *http.Request) map[string]string {
	names := map[string]string{}
	if list, err := s.dbm.store.Admins().List(r.Context()); err == nil {
		for _, a := range list {
			names[a.ID] = a.Username
		}
	}
	if list, err := s.dbm.db.Tickets().ListContacts(r.Context()); err == nil {
		for _, c := range list {
			names[c.ID] = c.Name
		}
	}
	return names
}

func actorName(names map[string]string, id string) string {
	if name := names[id]; name != "" {
		return name
	}
	return id
}

// auditFilterForActor lets a search by name also find what was recorded
// under that account's id.
func (s *Server) auditFilterForActor(r *http.Request, f repo.AuditFilter) repo.AuditFilter {
	if f.ActorID == "" {
		return f
	}
	if admin, err := s.dbm.store.Admins().ByUsername(r.Context(), f.ActorID); err == nil {
		f.ActorAltID = admin.ID
	}
	return f
}

func (s *Server) auditRows(r *http.Request, events []repo.AuditEvent) []auditRow {
	employees, devices := s.names(r)
	actors := s.actorNames(r)
	rows := make([]auditRow, 0, len(events))
	for _, ev := range events {
		rows = append(rows, auditRow{AuditEvent: ev, Actor: actorName(actors, ev.ActorID), Target: auditTargetName(ev, employees, devices, actors), Change: summariseChange(ev.Before, ev.After)})
	}
	return rows
}

// auditTargetName names what an event was done to: an employee by Windows
// user, a machine by hostname, an account by its current name. Anything
// else, or an id nobody answers to any more, is shown as recorded.
func auditTargetName(ev repo.AuditEvent, employees, devices, accounts map[string]string) string {
	names := map[string]map[string]string{"employee": employees, "device": devices, "admin": accounts, "customer_contact": accounts}[ev.TargetType]
	if name := names[ev.TargetID]; name != "" {
		return name
	}
	return ev.TargetID
}

// knownActions is what the action filter offers: every action the console
// writes, in the order the file that defines them lists them.
var knownActions = []string{
	"account.onboard", "account.reopen", "account.offboard", "account.offboard_sync", "account.reissue", "account.quota",
	"account.models", "account.profile", "account.delete",
	"machine.bind", "machine.unbind", "machine.codex_restart", "machine.sync",
	"policy.publish",
	"release.artifact_register", "release.artifact_status", "release.global_target",
	"release.rollout_create", "release.rollout_pause", "release.rollout_cancel",
	"release.target_exclude", "release.target_retry",
	"admin.signin", "admin.enrol", "admin.password_change", "admin.create", "admin.reset",
	"admin.disable", "admin.enable",
	"contact.create", "contact.role", "contact.password_reset", "contact.disable", "contact.enable",
}

// recordAccountEvent writes a change to a console or customer account into
// the audit trail, which the audit page searches and the worker ships to the
// log archive. The target is the account's id, so the record still names the
// right account after it is renamed. If the database will not take the event
// it goes straight to the log file instead: a sign-in or a reset that leaves
// no trace at all is worse than one recorded in the wrong place.
func (s *Server) recordAccountEvent(r *http.Request, actor, action, targetType, targetID string, detail map[string]any) {
	var raw []byte
	if len(detail) > 0 {
		raw, _ = json.Marshal(detail)
	}
	_, err := s.dbm.store.Audit().Append(r.Context(), repo.AuditEvent{
		ActorType: repo.ActorAdmin, ActorID: actor, Action: action,
		TargetType: targetType, TargetID: targetID, Detail: raw, RequestID: s.clientKey(r),
	})
	if err == nil {
		return
	}
	fields := map[string]any{"actor": actor, "target_type": targetType, "target": targetID, "audit_error": err.Error()}
	for k, v := range detail {
		fields[k] = v
	}
	s.events.Audit(action, action, fields)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "audit")
	f, fromText, toText, page := auditFilterFrom(r.URL.Query())
	f.Limit, f.Offset = auditPageSize, (page-1)*auditPageSize
	f = s.auditFilterForActor(r, f)
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
	f = s.auditFilterForActor(r, f)
	events, _, err := s.dbm.store.Audit().Search(r.Context(), f)
	if err != nil {
		http.Error(w, "導出失敗："+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	cw.Write([]string{"occurred_at", "actor_type", "actor", "action", "target_type", "target", "result", "request_id", "change", "before", "after"})
	employees, devices := s.names(r)
	actors := s.actorNames(r)
	written := 0
	for offset := 0; written < csvLimit; offset += 1000 {
		if offset > 0 {
			f.Offset = offset
			events, _, err = s.dbm.store.Audit().Search(r.Context(), f)
			if err != nil {
				// The status line is long gone; the file itself has to say
				// it is not the whole story.
				cw.Write([]string{"# 導出中斷：" + err.Error() + "，以上不是完整結果"})
				break
			}
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			target := auditTargetName(ev, employees, devices, actors)
			cw.Write([]string{ev.OccurredAt.Local().Format(time.RFC3339), ev.ActorType, csvSafe(actorName(actors, ev.ActorID)), csvSafe(ev.Action),
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
