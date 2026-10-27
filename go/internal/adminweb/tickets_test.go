package adminweb

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
)

func TestTicketRequestFields(t *testing.T) {
	form := url.Values{
		"subtype": {"employee"}, "employee_operation": {"onboard"},
		"employee_name_1": {"Alice"}, "work_email_1": {"alice@example.com"}, "company_department": {"Engineering"},
	}
	r := httptest.NewRequest("POST", "/customer/ticket/create", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	raw, err := ticketRequestDetails(r, "general")
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]any
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	if details["subtype"] != "employee" || details["schema_version"] != float64(3) || len(details["employees"].([]any)) != 1 {
		t.Fatalf("details=%v", details)
	}
	form.Set("employee_note_1", "sk-123456789012345678901234")
	r = httptest.NewRequest("POST", "/customer/ticket/create", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := ticketRequestDetails(r, "general"); err == nil {
		t.Fatal("credential was accepted")
	}
}

func TestTicketWebsiteAndBugFields(t *testing.T) {
	form := url.Values{"subtype": {"website"}, "block_websites": {"yes"}, "block_llm": {"yes"}, "llm_site": {"chatgpt", "deepseek"}, "website_name_1": {"example.com"}, "website_note_1": {"except support devices"}}
	r := httptest.NewRequest("POST", "/customer/ticket/create", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	raw, err := ticketRequestDetails(r, "general")
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]any
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	if len(details["llm_sites"].([]any)) != 2 || len(details["websites"].([]any)) != 1 {
		t.Fatalf("details=%v", details)
	}
	form.Set("llm_site", "unknown")
	r = httptest.NewRequest("POST", "/customer/ticket/create", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := ticketRequestDetails(r, "general"); err == nil {
		t.Fatal("unknown site was accepted")
	}
	bug := url.Values{"steps": {"open page"}, "expected": {"works"}, "actual": {"error"}}
	r = httptest.NewRequest("POST", "/customer/ticket/create", strings.NewReader(bug.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := ticketRequestDetails(r, "bug"); err != nil {
		t.Fatal(err)
	}
}

func ticketFormRequest(form url.Values) *http.Request {
	r := httptest.NewRequest("POST", "/customer/ticket/create", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestSoftwarePreinstallRows(t *testing.T) {
	form := url.Values{"subtype": {"software"}, "software_name_1": {"VS Code"}, "software_min_version_1": {"1.99"}, "software_note_1": {"Only Alice"}}
	raw, err := ticketRequestDetails(ticketFormRequest(form), "general")
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]any
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	items := details["software_items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["min_version"] != "1.99" {
		t.Fatalf("software items=%v", items)
	}
	form.Set("software_name_1", "")
	if _, err := ticketRequestDetails(ticketFormRequest(form), "general"); err == nil {
		t.Fatal("accepted a software row without a name")
	}
	form.Set("software_min_version_1", "")
	form.Set("software_note_1", "")
	form.Set("software_name_1", "无")
	if _, err := ticketRequestDetails(ticketFormRequest(form), "general"); err != nil {
		t.Fatalf("rejected no-software declaration: %v", err)
	}
	form.Set("software_name_2", "Python")
	if _, err := ticketRequestDetails(ticketFormRequest(form), "general"); err == nil {
		t.Fatal("accepted 无 alongside another package")
	}
}

func TestEnvironmentChangeAndConfirmationFields(t *testing.T) {
	form := url.Values{"subtype": {"environment"}, "change_application_date": {"2026-10-15"}, "change_type": {"website"}, "change_action": {"temporary_exception"}, "change_urgent": {"yes"}, "change_urgent_reason": {"Support incident"}, "change_application_note": {"Allow support site temporarily"}, "change_website_name_1": {"example.com"}}
	if _, err := ticketRequestDetails(ticketFormRequest(form), "general"); err == nil {
		t.Fatal("accepted temporary exception without cutoff date")
	}
	form.Set("change_website_cutoff_1", "2026-10-20")
	raw, err := ticketRequestDetails(ticketFormRequest(form), "general")
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]any
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	if details["change_type"] != "website" || len(details["change_items"].([]any)) != 1 {
		t.Fatalf("details=%v", details)
	}
	form.Set("change_type", "personnel")
	form.Set("change_person_name_1", "Alice")
	form.Set("change_person_email_1", "alice@example.com")
	if _, err := ticketRequestDetails(ticketFormRequest(form), "general"); err != nil {
		t.Fatalf("rejected personnel change: %v", err)
	}
	form.Set("change_person_email_1", "")
	if _, err := ticketRequestDetails(ticketFormRequest(form), "general"); err == nil {
		t.Fatal("accepted personnel row without work email")
	}
	confirmation := url.Values{"confirmation_result": {"partial"}, "confirmation_contact": {"Alice"}, "confirmation_signature": {"Alice"}, "confirmation_date": {"2026-10-15"}}
	if _, err := parseChangeConfirmation(ticketFormRequest(confirmation)); err != nil {
		t.Fatal(err)
	}
}

func TestTicketTemplatesRender(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	ticket := dbstore.Ticket{ID: "00000000-0000-0000-0000-000000000001", Title: "New employee", Details: map[string]any{"employee_name": "Alice"}, Version: 1}
	for _, tc := range []struct {
		name string
		data any
	}{
		{"tickets.html", pageData{DB: true, Role: "admin", Tickets: &ticketPage{Page: 1}}},
		{"ticket_detail.html", pageData{DB: true, Role: "admin", Tickets: &ticketPage{Ticket: &ticket}}},
		{"customer_login.html", customerPage{}},
		{"customer_home.html", customerPage{Page: 1}},
		{"customer_home.html", customerPage{AdminMode: true}},
		{"customer_portal.html", customerPage{Page: 1}},
		{"customer_portal.html", customerPage{Page: 1, NewView: true}},
		{"customer_ticket.html", customerPage{Ticket: &ticket}},
	} {
		var out bytes.Buffer
		if err := s.tpl.ExecuteTemplate(&out, tc.name, tc.data); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestAdminTicketFormUsesAdminEndpointWithoutContact(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	var out bytes.Buffer
	err := s.tpl.ExecuteTemplate(&out, "customer_home.html", customerPage{AdminMode: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`action="/tickets/create"`, `name="title"`, `name="subtype"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("admin form missing %q", want)
		}
	}
	if strings.Contains(out.String(), `action="/customer/logout"`) {
		t.Fatal("admin form showed customer logout")
	}
	if strings.Contains(out.String(), `name="requester_id"`) {
		t.Fatal("admin form still requires a customer contact")
	}
}

func TestTicketStructuredDetailsRender(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	for _, tc := range []struct {
		ticket dbstore.Ticket
		want   string
	}{
		{dbstore.Ticket{Type: "general", Details: map[string]any{"subtype": "employee", "employee_operation": "onboard", "employees": []any{map[string]any{"name": "Alice", "email": "alice@example.com"}}}}, "alice@example.com"},
		{dbstore.Ticket{Type: "general", Details: map[string]any{"subtype": "website", "block_websites": "yes", "block_llm": "yes", "llm_sites": []any{"chatgpt"}, "websites": []any{map[string]any{"name": "example.com"}}}}, "ChatGPT"},
		{dbstore.Ticket{Type: "bug", Details: map[string]any{"steps": "open page", "actual": "error"}}, "open page"},
	} {
		var out bytes.Buffer
		if err := s.tpl.ExecuteTemplate(&out, "customer_ticket.html", customerPage{Ticket: &tc.ticket}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("render missing %q", tc.want)
		}
	}
}

func TestEnvironmentWorkflowRendersForAdminAndCustomer(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	ticket := dbstore.Ticket{ID: "00000000-0000-0000-0000-000000000001", Type: "general", Title: "Allow support site", Number: 42, Version: 2, Details: map[string]any{"subtype": "environment", "change_type": "website", "change_action": "temporary_exception", "change_items": []any{map[string]any{"name": "example.com", "cutoff": "2026-10-20"}}}}
	workflow := dbstore.TicketChangeWorkflow{ImplementationID: 1, Implementation: map[string]any{"stage": "waiting_customer_result", "result_execution": "Allowed until Friday"}}
	for _, tc := range []struct {
		name string
		data any
		want string
	}{
		{"ticket_detail.html", pageData{DB: true, Role: "admin", Tickets: &ticketPage{Ticket: &ticket, Workflow: workflow}}, "实施方填写"},
		{"customer_ticket.html", customerPage{Ticket: &ticket, Workflow: workflow, Owner: true}, "客户结果确认"},
	} {
		var out bytes.Buffer
		if err := s.tpl.ExecuteTemplate(&out, tc.name, tc.data); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "example.com") {
			t.Fatalf("%s missing workflow or change item", tc.name)
		}
	}
}

func TestTicketConversationSeparatesPublicAndInternalActions(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	ticket := dbstore.Ticket{ID: "00000000-0000-0000-0000-000000000001", Type: "general", Title: "Request", RequesterID: "00000000-0000-0000-0000-000000000002", Version: 2, Details: map[string]any{}}
	events := []dbstore.TicketEvent{{ActorType: "customer", ActorName: "Alice", Kind: "comment", Visibility: "public", Body: "Please help"}, {ActorType: "admin", ActorName: "Operator", Kind: "comment", Visibility: "internal", Body: "Investigating"}}
	var out bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&out, "ticket_detail.html", pageData{DB: true, Role: "admin", Tickets: &ticketPage{Ticket: &ticket, Events: events}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`class="ticket-conversation`, `class="ticket-tools-title">内部备注与状态更新`, `class="ticket-tools-title">分派与关联`, `name="action" value="reply"`, `name="action" value="note"`, `客户可见`, `客户不可见`, `仅内部可见`, `Please help`, `Investigating`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("admin conversation missing %q", want)
		}
	}
	out.Reset()
	if err := s.tpl.ExecuteTemplate(&out, "customer_ticket.html", customerPage{Ticket: &ticket, Events: events[:1], Owner: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `ticket-customer-reply`) || strings.Contains(out.String(), `Investigating`) {
		t.Fatal("customer conversation rendered incorrectly")
	}
}

func TestProposalConfirmationRendersForManager(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	ticket := dbstore.Ticket{ID: "11111111-1111-1111-1111-111111111111", Type: "general", Title: "额度追加", Status: "waiting_customer", Version: 3,
		Details: map[string]any{"request_kind": "quota"}}
	proposal := &dbstore.TicketProposal{ID: 7, Kind: "quota", RequiresManager: true,
		Payload: map[string]any{"amount": "100000", "unit": "tokens", "valid_until": "2026-11-30", "cost": "12.50", "currency": "USD"}}
	var out bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&out, "customer_ticket.html", customerPage{Ticket: &ticket, Proposal: proposal, Manager: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `name="proposal_id" value="7"`) || !strings.Contains(out.String(), "12.50 USD") {
		t.Fatal("manager cannot confirm a priced quota proposal")
	}
	out.Reset()
	if err := s.tpl.ExecuteTemplate(&out, "customer_ticket.html", customerPage{Ticket: &ticket, Proposal: proposal, Manager: false, Owner: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `name="proposal_id" value="7"`) {
		t.Fatal("ordinary employee saw a paid proposal approval button")
	}
}

func TestTicketPendingBadgeAndFilterRender(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	var out bytes.Buffer
	data := pageData{DB: true, Role: "admin", Nav: "tickets", PendingTickets: 2, Tickets: &ticketPage{Status: "pending", Page: 1,
		Rows: []dbstore.Ticket{{ID: "00000000-0000-0000-0000-000000000001", Title: "Help", NeedsAction: true, Details: map[string]any{}}}}}
	if err := s.tpl.ExecuteTemplate(&out, "tickets.html", data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`href="/tickets?status=pending" class="active"`, `2 待处理`, `ticket-admin-card`, `待处理</span>`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ticket queue missing %q", want)
		}
	}
}

func TestAdminNewTicketUsesDailyPortal(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	var out bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&out, "customer_portal.html", customerPage{AdminMode: true, AdminPage: pageData{DB: true, Nav: "tickets"}, Manager: true, NewView: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`action="/tickets/create"`, `class="sheet tp-admin-main"`, `href="/tickets?status=pending"`, `name="request_kind"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("admin ticket portal missing %q", want)
		}
	}
}

func TestTicketRoles(t *testing.T) {
	if !allowed("viewer", "GET", "/tickets") {
		t.Fatal("viewer cannot read tickets")
	}
	if allowed("viewer", "POST", "/tickets/action") {
		t.Fatal("viewer can mutate tickets")
	}
	if !allowed("operator", "POST", "/tickets/action") {
		t.Fatal("operator cannot process tickets")
	}
	if allowed("operator", "POST", "/tickets/contacts/create") {
		t.Fatal("operator can create customer identity")
	}
	if !allowed("admin", "POST", "/tickets/contacts/reset") {
		t.Fatal("admin cannot reset customer password")
	}
	if allowed("operator", "POST", "/tickets/create") || allowed("viewer", "GET", "/tickets/new") {
		t.Fatal("non-admin can create a ticket on behalf of a customer")
	}
	if !allowed("admin", "POST", "/tickets/create") || !allowed("admin", "GET", "/tickets/new") {
		t.Fatal("admin cannot create a ticket on behalf of a customer")
	}
}
