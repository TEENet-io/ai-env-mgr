package dbstore

import (
	"context"
	"errors"
	"testing"
)

func TestDailyTicketRevisionAndProcessingBoundary(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	store := db.Tickets()
	owner, err := store.CreateContact(ctx, "owner@example.com", "Owner", "hash")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateContact(ctx, "other@example.com", "Other", "hash")
	if err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"schema_version":4,"request_kind":"software-add","software":"VS Code"}`)
	ticket, err := store.Create(ctx, owner.ID, "revision-test-key", "general", "安装软件 · VS Code", "软件：VS Code", "normal", first)
	if err != nil {
		t.Fatal(err)
	}
	second := []byte(`{"schema_version":4,"request_kind":"software-add","software":"Python"}`)
	if err := store.UpdateDailyRequest(ctx, ticket.ID, other.ID, 1, "general", "安装软件 · Python", "软件：Python", second); !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("other contact updated request: %v", err)
	}
	if err := store.UpdateDailyRequest(ctx, ticket.ID, owner.ID, 1, "general", "安装软件 · Python", "软件：Python", second); err != nil {
		t.Fatal(err)
	}
	revisions, err := store.RequestRevisions(ctx, ticket.ID, owner.ID)
	if err != nil || len(revisions) != 2 || revisions[0].Number != 2 || revisions[1].Number != 1 {
		t.Fatalf("revisions=%v error=%v", revisions, err)
	}
	if err := store.UpdateDailyRequest(ctx, ticket.ID, owner.ID, 1, "general", "stale", "stale", second); !errors.Is(err, ErrTicketConflict) {
		t.Fatalf("stale request updated: %v", err)
	}
	if err := store.AddEvent(ctx, ticket.ID, "", "admin", owner.ID, "status", "public", "开始处理", "working", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateDailyRequest(ctx, ticket.ID, owner.ID, 3, "general", "late", "late", second); !errors.Is(err, ErrTicketConflict) {
		t.Fatalf("in-progress request updated: %v", err)
	}
	var count int
	if err := db.Pool().QueryRow(ctx, `select count(*) from ticket_request_revisions where ticket_id=$1`, ticket.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("revision count=%d error=%v", count, err)
	}
}

func TestCustomerContactRoleChangeRevokesSessions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	store := db.Tickets()
	contact, err := store.CreateContactWithRole(ctx, "staff@example.com", "Staff", "hash", "employee")
	if err != nil || contact.Role != "employee" {
		t.Fatalf("contact=%+v error=%v", contact, err)
	}
	if err := store.CreateSession(ctx, contact.ID, []byte("session"), 60, 600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetContactRole(ctx, contact.ID, "manager"); err != nil {
		t.Fatal(err)
	}
	updated, err := store.ContactByEmail(ctx, contact.Email)
	if err != nil || updated.Role != "manager" {
		t.Fatalf("updated=%+v error=%v", updated, err)
	}
	var sessions int
	if err := db.Pool().QueryRow(ctx, `select count(*) from customer_sessions where contact_id=$1`, contact.ID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("sessions=%d error=%v", sessions, err)
	}
}
