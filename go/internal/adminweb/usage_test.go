package adminweb

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestUsageMonthAndMonthsBetween(t *testing.T) {
	now := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	label, from, to := usageMonth("", now)
	if label != "2026-09" || from.Day() != 1 || to.Month() != time.October {
		t.Fatalf("default month = %s %v %v", label, from, to)
	}
	label, from, to = usageMonth("2026-07", now)
	if label != "2026-07" || from.Month() != time.July || to.Month() != time.August {
		t.Fatalf("named month = %s %v %v", label, from, to)
	}
	if got := monthsBetween(time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)); strings.Join(got, ",") != "2026-09,2026-08,2026-07,2026-06" {
		t.Fatalf("months = %v", got)
	}
}

func TestUsagePageSumsAndExports(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	csrf := csrfFrom(t, s, cookie, "/users")
	for _, u := range []string{"work1", "work2"} {
		dbPost(t, h, "/users/onboard", url.Values{"csrf": {csrf}, "windowsUser": {u}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}, cookie)
	}
	work1, _ := s.dbm.store.Employees().ByWindowsUser(ctx, "work1")
	work2, _ := s.dbm.store.Employees().ByWindowsUser(ctx, "work2")
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.dbm.store.Usage().UpsertDay(ctx, day(3), []repo.UsageRow{
		{EmployeeID: work1.ID, KeyAlias: "a1", ModelGroup: "sonnet", Calls: 10, Failures: 1, CostUSD: "1.5", PromptTokens: 100, CompletionTokens: 20},
		{EmployeeID: work2.ID, KeyAlias: "a2", ModelGroup: "opus", Calls: 2, CostUSD: "3"},
	}))
	must(s.dbm.store.Usage().UpsertDay(ctx, day(4), []repo.UsageRow{
		{EmployeeID: work1.ID, KeyAlias: "a1", ModelGroup: "opus", Calls: 1, CostUSD: "0.5"},
	}))
	must(s.dbm.store.Usage().UpsertDay(ctx, day(5), []repo.UsageRow{
		{KeyAlias: "sk-stray", ModelGroup: "sonnet", Calls: 1, CostUSD: "0.01"},
	}))

	page := dbGet(t, h, "/usage?month=2026-09", cookie)
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatalf("usage page: %d", page.Code)
	}
	for _, want := range []string{">work1<", ">work2<", "sk-stray", "未关联到员工", "$5.0100", ">opus<", ">sonnet<", ">09-03<", ">09-05<", "调用 <b class=\"mono\">14</b>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	one := dbGet(t, h, "/usage?month=2026-09&employee=work2", cookie).Body.String()
	if strings.Contains(one, ">sonnet<") || !strings.Contains(one, ">opus<") || !strings.Contains(one, "$3.0000") {
		t.Fatalf("one employee's page shows other people's models")
	}
	if rec := dbGet(t, h, "/usage?month=2026-09&employee=nobody", cookie); rec.Code != 200 || !strings.Contains(rec.Body.String(), "没有这个员工") {
		t.Fatalf("unknown employee: %d", rec.Code)
	}

	csv := dbGet(t, h, "/usage.csv?month=2026-09", cookie)
	lines := strings.Split(strings.TrimSpace(csv.Body.String()), "\n")
	if csv.Code != 200 || len(lines) != 5 || !strings.HasPrefix(lines[0], "\xEF\xBB\xBFday,") || !strings.Contains(lines[1], "2026-09-03,work1,") {
		t.Fatalf("csv: %d %q", csv.Code, lines)
	}

	// The detail page carries the last 30 days only when they are recent;
	// September 2026 is in the past for this test's clock, so seed today.
	today := time.Now().UTC()
	must(s.dbm.store.Usage().UpsertDay(ctx, today, []repo.UsageRow{{EmployeeID: work1.ID, KeyAlias: "a1", ModelGroup: "sonnet", Calls: 7, CostUSD: "0.7"}}))
	detail := dbGet(t, h, "/users/detail?user=work1", cookie).Body.String()
	if !strings.Contains(detail, "最近 30 天用量") || !strings.Contains(detail, ">7<") || !strings.Contains(detail, "/usage?employee=work1") {
		t.Fatalf("detail page lacks the usage section")
	}

	// A viewer reads; only an operator can queue a snapshot.
	_, viewerPassword, err := s.dbm.auth.CreateAccount(ctx, "eve", "", "viewer")
	must(err)
	viewer := signInAs(t, s, "eve", viewerPassword)
	if rec := dbGet(t, h, "/usage", viewer); rec.Code != 200 {
		t.Fatalf("viewer usage: %d", rec.Code)
	}
	vcsrf := csrfFrom(t, s, viewer, "/usage")
	if rec := dbPost(t, h, "/usage/snapshot-now", url.Values{"csrf": {vcsrf}}, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer snapshot-now: %d, want 403", rec.Code)
	}
	if rec := dbPost(t, h, "/usage/snapshot-now", url.Values{"csrf": {csrf}}, cookie); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("admin snapshot-now: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	open, _ := s.dbm.store.Tasks().ListOpen(ctx, 20)
	queued := false
	for _, task := range open {
		queued = queued || task.Kind == "usage_snapshot"
	}
	if !queued {
		t.Fatal("the button queued nothing")
	}
}
