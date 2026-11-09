package adminweb

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The account tickets are about the AI workspace account, and say that the
// custom tool's account is not part of them.
func TestAccountTicketsNameTheAIWorkspaceAccount(t *testing.T) {
	s, _ := newDatabaseServer(t)
	manager := customerCookieFor(t, s, "lead@example.com", "manager")
	page := dbGet(t, s.Handler(), "/customer?view=new", manager).Body.String()
	for _, want := range []string{"<strong>AI 工作間帳號</strong>", "新增 AI 工作間帳號", "不包括定製工具帳號，定製工具帳號請管理員在工具後台辦理。"} {
		if !strings.Contains(page, want) {
			t.Errorf("the new-ticket page does not say %q", want)
		}
	}
	if strings.Contains(page, "員工帳號") {
		t.Error("the new-ticket page still says 員工帳號")
	}
}

// An offboarding ticket is not done until it says what happened to the
// employee's custom-tool account, and the customer sees that answer.
func TestAnOffboardingTicketNeedsTheCustomToolAccountResult(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	rootName, _ := knownAdmin(t, s)
	root := signedIn(t, s)
	ctx := t.Context()
	store := s.dbm.db.Tickets()
	admin, err := s.dbm.store.Admins().ByUsername(ctx, rootName)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := store.CreateContactWithRole(ctx, "lead@example.com", "Lead", "hash", "manager")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.Create(ctx, manager.ID, "offboard-key", "general", "員工離職／刪除帳號", "Alice 離職", "normal", []byte(`{"schema_version":4,"request_kind":"account-delete"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProposal(ctx, ticket.ID, admin.ID, 1, "account-delete", map[string]string{"deletion_scope": "AI 工作間帳號和定製工具帳號", "data_plan": "資料保留 7 天"}, true); err != nil {
		t.Fatal(err)
	}
	proposal, err := store.LatestProposal(ctx, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DecideProposal(ctx, ticket.ID, manager.ID, 2, proposal.ID, "approved"); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(dbGet(t, h, "/tickets/detail?id="+ticket.ID, root).Body.String(), `name="tool_account_result"`) == false {
		t.Fatal("the offboarding ticket's status form does not ask for the custom-tool account")
	}
	complete := func(result string) string {
		form := url.Values{"csrf": {csrfFrom(t, s, root, "/tickets")}, "id": {ticket.ID}, "version": {"3"}, "action": {"status"}, "status": {"done"}, "body": {"已關閉 AI 工作間帳號"}}
		if result != "" {
			form.Set("tool_account_result", result)
		}
		return dbPost(t, h, "/tickets/action", form, root).Header().Get("Location")
	}
	if loc := complete(""); !strings.Contains(loc, "err=") {
		t.Fatalf("an offboarding ticket was completed without the custom-tool result: %q", loc)
	}
	if got, _ := store.Get(ctx, ticket.ID, ""); got.Status == "done" {
		t.Fatal("the refused completion changed the ticket")
	}
	if loc := complete("已在工具後台停用 alice@example.com，讀回確認"); strings.Contains(loc, "err=") {
		t.Fatalf("completion with the result was refused: %q", loc)
	}
	events, err := store.Events(ctx, ticket.ID, manager.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if !strings.Contains(last.Body, "定製工具帳號：已在工具後台停用 alice@example.com") {
		t.Errorf("the customer does not see the custom-tool result: %q", last.Body)
	}
}

// The custom tool is one click away for operators and customers alike, when
// this console knows where it is; it is only a link, not a sign-in.
func TestTheCustomToolIsLinkedWhenConfigured(t *testing.T) {
	const tool = "https://tool.example.com/"
	link := `href="` + tool + `" target="_blank" rel="noopener"`

	s, _ := newDatabaseServerWith(t, func(o *Options) { o.ToolURL = tool })
	h := s.Handler()
	root := signedIn(t, s)
	manager := customerCookieFor(t, s, "lead@example.com", "manager")
	staff := customerCookieFor(t, s, "staff@example.com", "employee")
	for _, page := range []struct {
		path   string
		cookie *http.Cookie
	}{
		{"/overview", root}, {"/customer", manager}, {"/customer?view=new", staff}, {"/customer/usage", manager},
	} {
		if body := dbGet(t, h, page.path, page.cookie).Body.String(); !strings.Contains(body, link) {
			t.Errorf("%s has no link to the custom tool", page.path)
		}
	}

	plain, _ := newDatabaseServer(t)
	if strings.Contains(dbGet(t, plain.Handler(), "/overview", signedIn(t, plain)).Body.String(), "定製工具</a>") {
		t.Error("a console with no tool configured links to one")
	}
}
