package adminweb

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSummariseChangeNamesWhatDiffered(t *testing.T) {
	got := summariseChange([]byte(`{"budget":20,"models":["a"],"Version":3,"UpdatedAt":"t1","note":"x"}`), []byte(`{"budget":50,"models":["a","b"],"Version":4,"UpdatedAt":"t2","note":"x"}`))
	if got != `budget: 20 → 50; models: ["a"] → ["a","b"]` {
		t.Fatalf("summary = %q", got)
	}
	if got := summariseChange(nil, []byte(`{"hostname":"PC-1"}`)); got != "hostname: PC-1" {
		t.Fatalf("creation summary = %q", got)
	}
	if got := summariseChange([]byte(`{"a":1}`), nil); got != "a: 1 → —" {
		t.Fatalf("removal summary = %q", got)
	}
	if got := summariseChange(nil, nil); got != "" {
		t.Fatalf("nothing = %q", got)
	}
}

func TestAuditPageFiltersAndExportsCSV(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	csrf := csrfFrom(t, s, cookie, "/users")
	for _, u := range []string{"work1", "work2"} {
		dbPost(t, h, "/users/onboard", url.Values{"csrf": {csrf}, "windowsUser": {u}, "email": {"t@example.com"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}, cookie)
	}
	dbPost(t, h, "/users/quota", url.Values{"csrf": {csrf}, "windowsUser": {"work1"}, "email": {"t@example.com"}, "budget": {"50"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}, cookie)

	page := dbGet(t, h, "/audit?action=account.quota", cookie)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "共 1 条") || !strings.Contains(body, "account.quota") || !strings.Contains(body, "MonthlyBudget: 20.000000 → 50.000000") {
		t.Fatalf("filtered audit page: %d; count=%q rows=%q", page.Code, firstLine(body, "共 "), firstLine(body, "<tbody>"))
	}
	rows := body[strings.Index(body, "<tbody>"):strings.Index(body, "</tbody>")]
	if strings.Contains(rows, "account.onboard") {
		t.Fatal("the action filter let another action through")
	}
	// The target is shown by name, not by id.
	e, _ := s.dbm.store.Employees().ByWindowsUser(t.Context(), "work1")
	if !strings.Contains(body, ">work1<") || strings.Contains(body, ">"+e.ID+"<") {
		t.Fatal("the target must be the Windows user, not the uuid")
	}
	all := dbGet(t, h, "/audit", cookie)
	if !strings.Contains(all.Body.String(), "共 3 条") {
		t.Fatalf("unfiltered count missing; got %s", firstLine(all.Body.String(), "共 "))
	}
	admins, _ := s.dbm.store.Admins().List(t.Context())
	csv := dbGet(t, h, "/audit.csv?actor="+url.QueryEscape(admins[0].Username), cookie)
	if csv.Code != 200 || !strings.HasPrefix(csv.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("csv: %d %s", csv.Code, csv.Header().Get("Content-Type"))
	}
	lines := strings.Split(strings.TrimSpace(csv.Body.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "\xEF\xBB\xBFoccurred_at,") {
		t.Fatalf("csv has %d line(s); first %q", len(lines), lines[0])
	}
}

func firstLine(body, marker string) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	end := i + 40
	if end > len(body) {
		end = len(body)
	}
	return body[i:end]
}

func TestAuditCSVFailsLoudlyWhenTheSearchFails(t *testing.T) {
	s, _ := newDatabaseServer(t)
	cookie := signedIn(t, s)
	// PostgreSQL refuses a NUL byte in a text parameter, so this one filter
	// value makes the search itself fail while everything before it (the
	// session lookup) still works: the closest stand-in for a database that
	// stops answering half-way through a request.
	rec := dbGet(t, s.Handler(), "/audit.csv?actor=%00", cookie)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "导出失败") {
		t.Fatalf("a failed export must say so: %d %q", rec.Code, rec.Body.String())
	}
}

func TestAuditCSVNeutralisesFormulas(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	csrf := csrfFrom(t, s, cookie, "/users")
	req := httptest.NewRequest(http.MethodPost, "/users/onboard", strings.NewReader(url.Values{
		"csrf": {csrf}, "windowsUser": {"work1"}, "email": {"t@example.com"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Request-ID", "=1+1")
	req.AddCookie(cookie)
	h.ServeHTTP(httptest.NewRecorder(), req)

	csv := dbGet(t, h, "/audit.csv", cookie).Body.String()
	if strings.Contains(csv, ",=1+1,") {
		t.Fatalf("a cell starting with = is a formula to a spreadsheet:\n%s", csv)
	}
	if !strings.Contains(csv, ",'=1+1,") {
		t.Fatalf("the request id must still be readable, prefixed so it is text:\n%s", csv)
	}
}

func TestCSVSafe(t *testing.T) {
	cases := map[string]string{
		"=1+1": "'=1+1", "+3": "'+3", "-2": "'-2", "@cmd": "'@cmd", "\tx": "'\tx", "\rx": "'\rx",
		"plain": "plain", "": "", "工号 1": "工号 1", "2026-09-21T10:00:00+08:00": "2026-09-21T10:00:00+08:00",
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
