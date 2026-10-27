package adminweb

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
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
		{"desktop-problem", url.Values{"problem": {"远程桌面无法连接"}}, "bug"},
		{"software-add", url.Values{"employee_ids": {employees[0].ID, employees[1].ID}, "software": {"VS Code"}}, "general"},
		{"software-change", url.Values{"employee_ids": {employees[0].ID}, "software": {"VS Code"}, "software_action": {"update"}}, "general"},
		{"website", url.Values{"website_action": {"temporary"}, "sites": {"ChatGPT"}, "expires": {"2026-11-01T09:00"}}, "general"},
		{"ai-problem", url.Values{"problem": {"助手没有回复"}}, "bug"},
		{"quota", url.Values{}, "general"},
		{"tool-problem", url.Values{"tool": {"新闻监控"}, "problem": {"页面打不开"}}, "bug"},
		{"other-problem", url.Values{"problem": {"我不知道该如何分类"}}, "bug"},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			form := tc.fields
			form.Set("request_kind", tc.kind)
			form.Set("request_key", strings.Repeat("a", 32))
			kind, title, description, priority, key, raw, err := parseDailyTicketSubmission(ticketFormRequest(form), employees)
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
	deletion := url.Values{"proposal_kind": {"account-delete"}, "deletion_scope": {"账号与设备绑定"}, "data_plan": {"资料留存 30 天"}}
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
		if _, _, _, _, _, _, err := parseDailyTicketSubmission(ticketFormRequest(form), employees); err == nil {
			t.Errorf("accepted invalid %s: %v", tc.kind, tc.fields)
		}
	}
}
