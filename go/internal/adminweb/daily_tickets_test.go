package adminweb

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/oidc"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestDailyPortalRoleAndEditTemplate(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	var out bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&out, "customer_portal.html", customerPage{NewView: true, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `data-tp-category="account"`) {
		t.Fatal("employee contact saw account management category")
	}
	out.Reset()
	edit := dbstore.Ticket{ID: "11111111-1111-1111-1111-111111111111", Version: 2}
	if err := s.tpl.ExecuteTemplate(&out, "customer_portal.html", customerPage{NewView: true, Manager: true, EditTicket: &edit, EditPayload: `{"kind":"quota","details":{"category":"ai"}}`, Page: 1}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-tp-category="account"`, `action="/customer/ticket/edit"`, `name="version" value="2"`, `data-edit-json=`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("edit form missing %q", want)
		}
	}
}

func TestDailyTicketKinds(t *testing.T) {
	employees := []repo.Employee{
		{ID: "11111111-1111-1111-1111-111111111111", Name: "Alice", Email: "alice@example.com"},
		{ID: "22222222-2222-2222-2222-222222222222", Name: "Bob", Email: "bob@example.com"},
	}
	tests := []struct {
		kind     string
		fields   url.Values
		wantType string
	}{
		{"account-add", url.Values{"employee_name_1": {"Carol"}, "work_email_1": {"carol@example.com"}}, "general"},
		{"account-toggle", url.Values{"target_employee": {employees[0].ID}, "account_action": {"disable"}}, "general"},
		{"account-delete", url.Values{"target_employee": {employees[0].ID}}, "general"},
		{"account-edit", url.Values{"target_employee": {employees[0].ID}, "new_email": {"alice.new@example.com"}}, "general"},
		{"desktop-problem", url.Values{"problem": {"遠程桌面無法連接"}}, "bug"},
		{"software-add", url.Values{"employee_ids": {employees[0].ID, employees[1].ID}, "software": {"VS Code"}}, "general"},
		{"software-change", url.Values{"employee_ids": {employees[0].ID}, "software": {"VS Code"}, "software_action": {"update"}}, "general"},
		{"website", url.Values{"website_action": {"temporary"}, "sites": {"ChatGPT"}, "expires": {"2026-11-01T09:00"}}, "general"},
		{"ai-problem", url.Values{"problem": {"助手沒有回覆"}}, "bug"},
		{"quota", url.Values{"employee_ids": {employees[0].ID, employees[1].ID}}, "general"},
		{"tool-problem", url.Values{"tool": {"新聞監控"}, "problem": {"頁面打不開"}}, "bug"},
		{"other-problem", url.Values{"problem": {"我不知道該如何分類"}}, "bug"},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			form := tc.fields
			form.Set("request_kind", tc.kind)
			form.Set("request_key", strings.Repeat("a", 32))
			kind, title, description, priority, key, raw, err := parseDailyTicketSubmission(ticketFormRequest(form), employees, nil)
			if err != nil {
				t.Fatal(err)
			}
			if kind != tc.wantType || title == "" || description == "" || priority != "normal" || key != strings.Repeat("a", 32) {
				t.Fatalf("unexpected ticket: %s %q %q %s %s", kind, title, description, priority, key)
			}
			var details map[string]any
			if err := json.Unmarshal(raw, &details); err != nil {
				t.Fatal(err)
			}
			if details["request_kind"] != tc.kind || details["schema_version"] != float64(4) {
				t.Fatalf("details=%v", details)
			}
		})
	}
}

func TestDailyEmployeeScope(t *testing.T) {
	employees := []repo.Employee{
		{ID: "one", Name: "Alice", Email: "alice@example.com"},
		{ID: "two", Name: "Bob", Email: "bob@example.com"},
	}
	if got := dailyAuthorizedEmployees(employees, dbstore.CustomerContact{Role: "employee", Email: "ALICE@example.com"}); len(got) != 1 || got[0].ID != "one" {
		t.Fatalf("employee scope=%v", got)
	}
	if got := dailyAuthorizedEmployees(employees, dbstore.CustomerContact{Role: "manager"}); len(got) != 2 {
		t.Fatalf("manager scope=%v", got)
	}
}

func TestDailyProposalValidation(t *testing.T) {
	quota := url.Values{"proposal_kind": {"quota"}, "amount": {"100000"}, "unit": {"tokens"},
		"valid_until": {"2026-11-30"}, "cost": {"0"}, "currency": {"USD"}}
	kind, _, manager, err := parseDailyProposal(ticketFormRequest(quota))
	if err != nil || kind != "quota" || manager {
		t.Fatalf("free quota proposal: kind=%s manager=%v error=%v", kind, manager, err)
	}
	quota.Set("cost", "12.50")
	_, _, manager, err = parseDailyProposal(ticketFormRequest(quota))
	if err != nil || !manager {
		t.Fatalf("paid quota proposal did not require manager: %v", err)
	}
	quota.Set("cost", "NaN")
	if _, _, _, err := parseDailyProposal(ticketFormRequest(quota)); err == nil {
		t.Fatal("accepted nonnumeric cost")
	}
	deletion := url.Values{"proposal_kind": {"account-delete"}, "deletion_scope": {"帳號與設備綁定"}, "data_plan": {"資料留存 30 天"}}
	_, _, manager, err = parseDailyProposal(ticketFormRequest(deletion))
	if err != nil || !manager {
		t.Fatalf("deletion plan did not require manager: %v", err)
	}
}

func TestDailyTicketRejectsInvalidTargetAndFields(t *testing.T) {
	employees := []repo.Employee{{ID: "11111111-1111-1111-1111-111111111111", Name: "Alice", Email: "alice@example.com"}}
	for _, tc := range []struct {
		kind   string
		fields url.Values
	}{
		{"account-add", url.Values{"employee_name_1": {"Alice"}}},
		{"account-toggle", url.Values{"target_employee": {"unknown"}, "account_action": {"disable"}}},
		{"account-edit", url.Values{"target_employee": {employees[0].ID}, "new_name": {"Alice"}}},
		{"software-add", url.Values{"software": {"VS Code"}}},
		{"software-add", url.Values{"employee_ids": {employees[0].ID, employees[0].ID}, "software": {"VS Code"}}},
		{"website", url.Values{"website_action": {"temporary"}, "sites": {"ChatGPT"}}},
		{"website", url.Values{"website_action": {"block"}, "sites": {"unknown"}}},
		{"desktop-problem", url.Values{"problem": {""}}},
		{"tool-problem", url.Values{"problem": {"page fails"}}},
	} {
		form := tc.fields
		form.Set("request_kind", tc.kind)
		form.Set("request_key", strings.Repeat("a", 32))
		if _, _, _, _, _, _, err := parseDailyTicketSubmission(ticketFormRequest(form), employees, nil); err == nil {
			t.Errorf("accepted invalid %s: %v", tc.kind, tc.fields)
		}
	}
}

func TestContactsRaiseOnlyTheKindsTheirRoleAllows(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	employee := customerCookieFor(t, s, "staff@example.com", "employee")
	manager := customerCookieFor(t, s, "lead@example.com", "manager")
	withWorkspace(t, s, "staff@example.com")
	raise := func(cookie *http.Cookie, kind string) *httptest.ResponseRecorder {
		t.Helper()
		key, err := randomTicketKey()
		if err != nil {
			t.Fatal(err)
		}
		return postTicketForm(t, h, "/customer/ticket/create", cookie, map[string]string{
			"csrf": s.csrfFor(cookie.Value), "request_key": key, "request_kind": kind, "problem": "它打不開",
		})
	}
	for kind, spec := range dailyRequests {
		want := spec.Category != "account" && kind != "quota"
		if got := contactMayRequest("employee", kind); got != want {
			t.Errorf("employee may raise %s = %v, want %v", kind, got, want)
		}
		if !contactMayRequest("manager", kind) {
			t.Errorf("manager may not raise %s", kind)
		}
	}
	for _, kind := range []string{"quota", "account-add", "account-delete"} {
		if rec := raise(employee, kind); rec.Code != http.StatusForbidden {
			t.Errorf("employee raised %s: %d", kind, rec.Code)
		}
	}
	for _, kind := range []string{"desktop-problem", "ai-problem"} {
		if rec := raise(employee, kind); rec.Code != http.StatusSeeOther {
			t.Errorf("employee raising %s: %d %s", kind, rec.Code, rec.Body)
		}
	}
	if rec := raise(manager, "ai-problem"); rec.Code != http.StatusSeeOther {
		t.Errorf("manager raising ai-problem: %d %s", rec.Code, rec.Body)
	}

	page := dbGet(t, h, "/customer?view=new", employee).Body.String()
	for _, hidden := range []string{`data-tp-category="account"`, `data-tp-kind="quota"`} {
		if strings.Contains(page, hidden) {
			t.Errorf("employee's form offers %s", hidden)
		}
	}
	for _, shown := range []string{`data-tp-category="desktop"`, `data-tp-category="ai"`, `data-tp-category="tool"`, `data-tp-category="other"`} {
		if !strings.Contains(page, shown) {
			t.Errorf("employee's form lacks %s", shown)
		}
	}
	if home := dbGet(t, h, "/customer", employee).Body.String(); strings.Contains(home, "category=account") || !strings.Contains(home, "category=ai") {
		t.Error("employee's home page offers a kind they cannot raise")
	}
}

func TestAnEditCannotMoveToAKindTheContactMayNotRaise(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	employee := customerCookieFor(t, s, "staff@example.com", "employee")
	withWorkspace(t, s, "staff@example.com")
	key, err := randomTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	rec := postTicketForm(t, h, "/customer/ticket/create", employee, map[string]string{
		"csrf": s.csrfFor(employee.Value), "request_key": key, "request_kind": "desktop-problem", "problem": "它打不開",
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	id := strings.SplitN(rec.Header().Get("Location"), "id=", 2)[1]
	rec = postTicketForm(t, h, "/customer/ticket/edit", employee, map[string]string{
		"csrf": s.csrfFor(employee.Value), "id": id, "version": "1", "request_kind": "quota", "problem": "它打不開",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("edit into quota: %d %s", rec.Code, rec.Body)
	}
}

func TestAManagerReadsUsageAndEmployeesOfThisCompanyOnly(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	ctx := t.Context()
	employee := customerCookieFor(t, s, "staff@example.com", "employee")
	manager := customerCookieFor(t, s, "lead@example.com", "manager")
	ours, err := s.dbm.store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1", Name: "張三", Email: "zhang@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	if err := s.dbm.store.Usage().UpsertDay(ctx, day, []repo.UsageRow{
		{EmployeeID: ours.ID, KeyAlias: "bippo-emp-work1-e1", ModelGroup: "gpt-5.5", Calls: 3, CostUSD: "12.3456", PromptTokens: 100, CompletionTokens: 20},
		{KeyAlias: "wewe-emp-other-e1", ModelGroup: "other-company-model", Calls: 900, CostUSD: "99", PromptTokens: 7777},
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/customer/usage", "/customer/employees"} {
		if rec := dbGet(t, h, path); rec.Code != http.StatusSeeOther {
			t.Errorf("signed out %s: %d", path, rec.Code)
		}
		if rec := dbGet(t, h, path, employee); rec.Code != http.StatusForbidden {
			t.Errorf("employee %s: %d", path, rec.Code)
		}
	}
	if strings.Contains(dbGet(t, h, "/customer", employee).Body.String(), "/customer/usage") {
		t.Error("an employee is offered the usage page")
	}
	if !strings.Contains(dbGet(t, h, "/customer", manager).Body.String(), "/customer/usage") {
		t.Error("a manager is not offered the usage page")
	}

	usage := dbGet(t, h, "/customer/usage", manager)
	if usage.Code != http.StatusOK {
		t.Fatalf("manager usage: %d", usage.Code)
	}
	body := usage.Body.String()
	for _, want := range []string{"張三", "gpt-5.5", "120"} {
		if !strings.Contains(body, want) {
			t.Errorf("usage lacks %q", want)
		}
	}
	for _, leak := range []string{"other-company-model", "wewe-emp", "7777", "12.3456"} {
		if strings.Contains(body, leak) {
			t.Errorf("usage shows %q", leak)
		}
	}
	people := dbGet(t, h, "/customer/employees", manager).Body.String()
	if !strings.Contains(people, "zhang@example.com") || !strings.Contains(people, "work1") {
		t.Error("the employee list lacks the employee")
	}
}

func TestAQuotaRequestNamesItsEmployees(t *testing.T) {
	employees := []repo.Employee{{ID: "11111111-1111-1111-1111-111111111111", Name: "Alice", Email: "alice@example.com"}}
	form := url.Values{"request_kind": {"quota"}, "request_key": {strings.Repeat("a", 32)}}
	if _, _, _, _, _, _, err := parseDailyTicketSubmission(ticketFormRequest(form), employees, nil); err == nil {
		t.Fatal("a quota request without an employee was accepted")
	}
	form.Set("employee_ids", employees[0].ID)
	_, _, description, _, _, raw, err := parseDailyTicketSubmission(ticketFormRequest(form), employees, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description, "Alice") || !strings.Contains(string(raw), employees[0].ID) {
		t.Fatalf("the employee is not on the request: %s / %s", description, raw)
	}
}

// An employee without an AI workspace account has no remote desktop or AI
// to ask about: those requests are neither offered nor accepted (AIC-204
// #10). A manager raises them for others and is not limited.
func TestAnEmployeeWithoutAnAccountSeesNoDesktopOrAIRequests(t *testing.T) {
	// Without the custom tool's list, contacts and accounts meet only by
	// email, and nothing is taken away.
	legacy, _ := newDatabaseServer(t)
	nobody := customerCookieFor(t, legacy, "staff@example.com", "employee")
	key, _ := randomTicketKey()
	if rec := postTicketForm(t, legacy.Handler(), "/customer/ticket/create", nobody, map[string]string{
		"csrf": legacy.csrfFor(nobody.Value), "request_key": key, "request_kind": "ai-problem", "problem": "它打不開",
	}); rec.Code != http.StatusSeeOther {
		t.Errorf("without the list, ai-problem: %d", rec.Code)
	}

	s := newDirectoryServer(t)
	h := s.Handler()
	employee := customerCookieFor(t, s, "staff@example.com", "employee")
	manager := customerCookieFor(t, s, "lead@example.com", "manager")
	raise := func(cookie *http.Cookie, kind string) int {
		t.Helper()
		key, _ := randomTicketKey()
		return postTicketForm(t, h, "/customer/ticket/create", cookie, map[string]string{
			"csrf": s.csrfFor(cookie.Value), "request_key": key, "request_kind": kind, "problem": "它打不開", "tool": "",
		}).Code
	}
	for _, kind := range []string{"desktop-problem", "ai-problem", "website"} {
		if code := raise(employee, kind); code != http.StatusForbidden {
			t.Errorf("employee without an account raised %s: %d", kind, code)
		}
	}
	if code := raise(employee, "other-problem"); code != http.StatusSeeOther {
		t.Errorf("other-problem: %d", code)
	}
	if code := raise(manager, "desktop-problem"); code != http.StatusSeeOther {
		t.Errorf("manager desktop-problem: %d", code)
	}
	page := dbGet(t, h, "/customer?view=new", employee).Body.String()
	for _, hidden := range []string{`data-tp-category="desktop"`, `data-tp-category="ai"`} {
		if strings.Contains(page, hidden) {
			t.Errorf("the form offers %s", hidden)
		}
	}
	if home := dbGet(t, h, "/customer", employee).Body.String(); strings.Contains(home, "category=desktop") || strings.Contains(home, "category=ai") {
		t.Error("the home page offers desktop or AI")
	}
	// An account under the same email changes that; so does a closed one going back.
	emp, err := s.dbm.store.Employees().Create(t.Context(), repo.NewEmployee{WindowsUser: "work1", Email: "staff@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if code := raise(employee, "ai-problem"); code != http.StatusSeeOther {
		t.Errorf("with an account, ai-problem: %d", code)
	}
	if _, err := s.dbm.store.Employees().Offboard(t.Context(), emp.ID, emp.Version); err != nil {
		t.Fatal(err)
	}
	if code := raise(employee, "ai-problem"); code != http.StatusForbidden {
		t.Errorf("after the account closed, ai-problem: %d", code)
	}
}

// withWorkspace gives a contact an AI workspace account under the same
// email, for requests only somebody with one may raise.
func withWorkspace(t *testing.T, s *Server, email string) {
	t.Helper()
	user := "w-" + strings.SplitN(email, "@", 2)[0]
	if _, err := s.dbm.store.Employees().Create(t.Context(), repo.NewEmployee{WindowsUser: user, Email: email}); err != nil {
		t.Fatal(err)
	}
}

func TestAViewerReadsTheCompanyButRaisesOnlyWhatAnEmployeeMay(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	viewer := customerCookieFor(t, s, "it@example.com", "viewer")
	for _, path := range []string{"/customer/usage", "/customer/employees"} {
		if rec := dbGet(t, h, path, viewer); rec.Code != http.StatusOK {
			t.Errorf("viewer %s: %d", path, rec.Code)
		}
	}
	home := dbGet(t, h, "/customer", viewer).Body.String()
	if !strings.Contains(home, "/customer/usage") {
		t.Error("a viewer is not offered the usage page")
	}
	if strings.Contains(home, "category=account") {
		t.Error("a viewer is offered AI workspace account requests")
	}
	if !contactMayRequest("manager", "quota") || contactMayRequest("viewer", "quota") || contactMayRequest("viewer", "account-delete") {
		t.Error("only a manager may ask for quota or accounts")
	}
}

func TestAnOperatorAtTheCustomerEntryIsSentToTheOpsEntry(t *testing.T) {
	s, _ := newDatabaseServer(t)
	rec := httptest.NewRecorder()
	s.oidcCustomerSignIn(rec, httptest.NewRequest(http.MethodGet, "/oidc/callback", nil),
		oidc.Claims{Subject: "00000000-0000-4000-8000-000000000009", PreferredUsername: "wei", Role: "ops"})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/oidc/start?entry=ops" {
		t.Fatalf("ops at the customer entry: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
