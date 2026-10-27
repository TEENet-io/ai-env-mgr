package dbstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestTicketOwnershipIdempotencyAndTimeline(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := db.Tickets()
	a, err := s.CreateContact(ctx, "a@example.com", "Alice", "hash")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateContact(ctx, "b@example.com", "Bob", "hash")
	if err != nil {
		t.Fatal(err)
	}
	one, err := s.Create(ctx, a.ID, "same-request-key", "onboarding", "New employee", "Please onboard", "normal", []byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Create(ctx, a.ID, "same-request-key", "onboarding", "Changed title", "Changed description", "normal", []byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if one.ID != again.ID {
		t.Fatal("retry created a second ticket")
	}
	if _, err := s.Get(ctx, one.ID, b.ID); !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("other contact read ticket: %v", err)
	}
	if _, err := s.Events(ctx, one.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEvent(ctx, one.ID, b.ID, "customer", b.ID, "comment", "public", "forged", "", 1); !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("other contact replied: %v", err)
	}
	if err := s.AddEvent(ctx, one.ID, "", "admin", a.ID, "comment", "internal", "secret internal note", "", 1); err != nil {
		t.Fatal(err)
	}
	public, err := s.Events(ctx, one.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(public) != 1 {
		t.Fatalf("customer saw %d events; want only creation", len(public))
	}
	if err := s.AddEvent(ctx, one.ID, "", "admin", a.ID, "status", "public", "We are working on it", "working", 1); !errors.Is(err, ErrTicketConflict) {
		t.Fatalf("stale version succeeded: %v", err)
	}
	if err := s.AddEvent(ctx, one.ID, "", "admin", a.ID, "status", "public", "We are working on it", "working", 2); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, one.ID, a.ID)
	if err != nil || got.Status != "working" || got.Version != 3 {
		t.Fatalf("state=%+v, err=%v", got, err)
	}
	if _, err := db.Pool().Exec(ctx, `insert into employees(windows_user) values('alice')`); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkSubject(ctx, one.ID, "employee", "alice", a.ID, 3); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, one.ID, a.ID)
	if err != nil || got.EmployeeWindowsUser != "alice" {
		t.Fatalf("employee link=%+v, err=%v", got, err)
	}
	var adminID string
	if err := db.Pool().QueryRow(ctx, `insert into admin_principals(username,password_hash,role) values('ticket_operator','hash','operator') returning id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `insert into admin_jobs(id,kind,state) values('job-ticket-test','test','done')`); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkTask(ctx, one.ID, "admin", "job-ticket-test", adminID); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkTask(ctx, one.ID, "admin", "job-ticket-test", adminID); err != nil {
		t.Fatal(err)
	}
	links, err := s.TaskIDs(ctx, one.ID)
	if err != nil || len(links) != 1 || links[0].ID != "job-ticket-test" {
		t.Fatalf("links=%v, err=%v", links, err)
	}
	var auditCount, deliveryCount int
	if err := db.Pool().QueryRow(ctx, `select count(*),count(ed.event_id) from audit_events ae
		left join event_deliveries ed on ed.event_id=ae.event_id where ae.target_type='ticket' and ae.target_id=$1`, one.ID).
		Scan(&auditCount, &deliveryCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 5 || deliveryCount != 5 {
		t.Fatalf("audit=%d, queued=%d; want 5 each", auditCount, deliveryCount)
	}
}

func TestAdminCreatedTicketAttributesActualAuthor(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := db.Pool().QueryRow(ctx, `insert into admin_principals(username,password_hash,role) values('ticket_admin','hash','admin') returning id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	ticket, err := db.Tickets().CreateByAdmin(ctx, "admin-request-key", "bug", "A bug", "Repro steps", "normal", []byte(`{"schema_version":2}`), adminID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := db.Tickets().CreateByAdmin(ctx, "admin-request-key", "bug", "Changed", "Changed", "normal", []byte(`{"schema_version":2}`), adminID)
	if err != nil || again.ID != ticket.ID {
		t.Fatalf("admin retry created a different ticket: first=%+v again=%+v err=%v", ticket, again, err)
	}
	got, err := db.Tickets().Get(ctx, ticket.ID, "")
	if err != nil || got.RequesterID != "" || got.RequesterName != "内部工单" {
		t.Fatalf("requester=%q name=%q, err=%v", got.RequesterID, got.RequesterName, err)
	}
	contact, err := db.Tickets().CreateContact(ctx, "requester@example.com", "Requester", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Tickets().Get(ctx, ticket.ID, contact.ID); !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("customer read internal ticket: %v", err)
	}
	events, err := db.Tickets().Events(ctx, ticket.ID, "")
	if err != nil || len(events) != 1 || events[0].ActorType != "admin" || events[0].ActorName != "ticket_admin" {
		t.Fatalf("events=%v, err=%v", events, err)
	}
	if err := db.Tickets().AddEvent(ctx, ticket.ID, "", "admin", adminID, "comment", "internal", "working", "", 1); err != nil {
		t.Fatalf("admin cannot update internal ticket: %v", err)
	}
	var actorType, actorID string
	if err := db.Pool().QueryRow(ctx, `select actor_type,actor_id from audit_events where target_type='ticket' and target_id=$1 and action='ticket_created'`, ticket.ID).Scan(&actorType, &actorID); err != nil {
		t.Fatal(err)
	}
	if actorType != "admin" || actorID != adminID {
		t.Fatalf("audit actor=%s/%s", actorType, actorID)
	}
}

func TestCustomerSessionExpiresAndDisablingRevokesIt(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := db.Tickets()
	c, err := s.CreateContact(ctx, "a@example.com", "Alice", "hash")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("test token"))
	if err := s.CreateSession(ctx, c.ID, hash[:], time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContactBySession(ctx, hash[:], time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContactDisabled(ctx, c.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContactBySession(ctx, hash[:], time.Minute); err == nil {
		t.Fatal("disabled contact retained session")
	}
}

func TestTicketTransitions(t *testing.T) {
	for _, tc := range []struct {
		from, to, actor string
		want            bool
	}{
		{"new", "closed", "customer", true},
		{"working", "closed", "customer", false},
		{"waiting_customer", "working", "customer", true},
		{"done", "new", "customer", true},
		{"new", "done", "admin", false},
		{"working", "done", "admin", true},
		{"closed", "new", "admin", false},
	} {
		if got := validTicketTransition(tc.from, tc.to, tc.actor); got != tc.want {
			t.Errorf("%s -> %s by %s = %t, want %t", tc.from, tc.to, tc.actor, got, tc.want)
		}
	}
}

func TestTicketPrivilegeGrid(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var appCanUpdateEvents, roCanReadContacts bool
	if err := db.Pool().QueryRow(ctx, `select has_table_privilege('aienv_app','ticket_events','UPDATE'),
		has_table_privilege('aienv_ro','customer_contacts','SELECT')`).Scan(&appCanUpdateEvents, &roCanReadContacts); err != nil {
		t.Fatal(err)
	}
	if appCanUpdateEvents || roCanReadContacts {
		t.Fatalf("unsafe grants: app update events=%t, read-only contacts=%t", appCanUpdateEvents, roCanReadContacts)
	}
}

func TestTicketActionRequiredFollowsCustomerReplies(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := db.Tickets()
	c, err := s.CreateContact(ctx, "pending@example.com", "Customer", "hash")
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := db.Pool().QueryRow(ctx, `insert into admin_principals(username,password_hash,role) values('pending_operator','hash','operator') returning id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	ticket, err := s.Create(ctx, c.ID, "pending-key", "general", "Help", "Need help", "normal", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		count, err := s.ActionRequiredCount(ctx)
		if err != nil || count != want {
			t.Fatalf("pending count=%d, want %d, err=%v", count, want, err)
		}
		rows, err := s.List(ctx, "", "pending", 10, 0)
		if err != nil || len(rows) != want || (want > 0 && !rows[0].NeedsAction) {
			t.Fatalf("pending rows=%+v, want %d, err=%v", rows, want, err)
		}
	}
	check(1)
	if err := s.AddEvent(ctx, ticket.ID, "", "admin", adminID, "status", "public", "Started", "working", 1); err != nil {
		t.Fatal(err)
	}
	check(0)
	if err := s.AddEvent(ctx, ticket.ID, c.ID, "customer", c.ID, "comment", "public", "More detail", "", 2); err != nil {
		t.Fatal(err)
	}
	check(1)
	if err := s.AddEvent(ctx, ticket.ID, "", "admin", adminID, "comment", "internal", "Investigating", "", 3); err != nil {
		t.Fatal(err)
	}
	check(1) // A private note is not a customer-facing answer.
	if err := s.AddEvent(ctx, ticket.ID, "", "admin", adminID, "comment", "public", "We are on it", "", 4); err != nil {
		t.Fatal(err)
	}
	check(0)
	if err := s.AddEvent(ctx, ticket.ID, c.ID, "customer", c.ID, "comment", "public", "Another question", "", 5); err != nil {
		t.Fatal(err)
	}
	check(1)
	if err := s.AddEvent(ctx, ticket.ID, "", "admin", adminID, "status", "public", "Resolved", "done", 6); err != nil {
		t.Fatal(err)
	}
	check(0)
	if err := s.AddEvent(ctx, ticket.ID, c.ID, "customer", c.ID, "comment", "public", "Still broken", "new", 7); err != nil {
		t.Fatal(err)
	}
	check(1)
}

func TestEnvironmentChangeWorkflow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := db.Tickets()
	owner, err := s.CreateContact(ctx, "change-owner@example.com", "Owner", "hash")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateContact(ctx, "change-other@example.com", "Other", "hash")
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := db.Pool().QueryRow(ctx, `insert into admin_principals(username,password_hash,role) values('change_operator','hash','operator') returning id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	ticket, err := s.Create(ctx, owner.ID, "environment-change-key", "general", "Temporary exception", "Please allow example.com", "normal", []byte(`{"schema_version":3,"subtype":"environment"}`))
	if err != nil {
		t.Fatal(err)
	}
	implementation := map[string]string{"stage": "waiting_customer_result", "result_execution": "Allowed until Friday"}
	if err := s.SaveChangeImplementation(ctx, ticket.ID, adminID, 1, implementation); err != nil {
		t.Fatal(err)
	}
	workflow, err := s.ChangeWorkflow(ctx, ticket.ID)
	if err != nil || !workflow.AwaitingConfirmation() {
		t.Fatalf("workflow=%+v err=%v", workflow, err)
	}
	confirmation := map[string]string{"result": "completed", "contact": "Owner", "signature": "Owner", "date": "2026-10-15"}
	if err := s.ConfirmChangeResult(ctx, ticket.ID, other.ID, 2, confirmation); !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("other customer confirmed: %v", err)
	}
	if err := s.ConfirmChangeResult(ctx, ticket.ID, owner.ID, 2, confirmation); err != nil {
		t.Fatal(err)
	}
	workflow, err = s.ChangeWorkflow(ctx, ticket.ID)
	if err != nil || workflow.AwaitingConfirmation() || workflow.Confirmation["result"] != "completed" {
		t.Fatalf("confirmed workflow=%+v err=%v", workflow, err)
	}
	if err := s.ConfirmChangeResult(ctx, ticket.ID, owner.ID, 3, confirmation); err == nil {
		t.Fatal("accepted duplicate confirmation")
	}
	if err := s.SaveChangeImplementation(ctx, ticket.ID, adminID, 3, map[string]string{"stage": "verifying"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool().QueryRow(ctx, `select count(*) from ticket_change_entries where ticket_id=$1`, ticket.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("entries=%d err=%v", count, err)
	}
}
