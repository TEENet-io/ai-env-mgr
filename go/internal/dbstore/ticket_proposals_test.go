package dbstore

import (
	"context"
	"errors"
	"testing"
)

func TestQuotaProposalRequiresManagerConfirmationBeforeCompletion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	store := db.Tickets()
	employee, err := store.CreateContactWithRole(ctx, "quota-staff@example.com", "Staff", "hash", "employee")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := store.CreateContactWithRole(ctx, "quota-manager@example.com", "Manager", "hash", "manager")
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := db.Pool().QueryRow(ctx, `insert into admin_principals(username,password_hash,role)
		values('quota_operator','hash','operator') returning id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.Create(ctx, employee.ID, "quota-proposal-key", "general", "额度用完，申请增加", "申请追加额度", "normal", []byte(`{"schema_version":4,"request_kind":"quota"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetForContact(ctx, ticket.ID, manager.ID, manager.Role); err != nil {
		t.Fatalf("manager could not view employee request: %v", err)
	}
	if _, err := store.GetForContact(ctx, ticket.ID, manager.ID, "employee"); !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("unprivileged contact viewed ticket: %v", err)
	}
	if err := store.AddEvent(ctx, ticket.ID, "", "admin", adminID, "status", "public", "开始核对", "working", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEvent(ctx, ticket.ID, "", "admin", adminID, "status", "public", "完成", "done", 2); err == nil {
		t.Fatal("completed a quota request without a proposal")
	}
	payload := map[string]string{"amount": "100000", "unit": "tokens", "valid_until": "2026-11-30", "cost": "12.50", "currency": "USD"}
	if err := store.SaveProposal(ctx, ticket.ID, adminID, 2, "quota", payload, true); err != nil {
		t.Fatal(err)
	}
	withProposal, err := store.Get(ctx, ticket.ID, employee.ID)
	if err != nil || withProposal.StatusLabel() != "待联系人确认方案" {
		t.Fatalf("ticket status=%+v error=%v", withProposal, err)
	}
	proposal, err := store.LatestProposal(ctx, ticket.ID)
	if err != nil || !proposal.Awaiting() || !proposal.RequiresManager {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	approvals, err := store.PendingManagerApprovals(ctx, 20)
	if err != nil || len(approvals) != 1 || approvals[0].TicketID != ticket.ID {
		t.Fatalf("approvals=%v err=%v", approvals, err)
	}
	if err := store.DecideProposal(ctx, ticket.ID, employee.ID, 3, proposal.ID, "approved"); !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("employee approved paid proposal: %v", err)
	}
	if err := store.DecideProposal(ctx, ticket.ID, manager.ID, 3, proposal.ID, "approved"); err != nil {
		t.Fatal(err)
	}
	if err := store.DecideProposal(ctx, ticket.ID, manager.ID, 3, proposal.ID, "approved"); !errors.Is(err, ErrTicketConflict) {
		t.Fatalf("duplicate approval succeeded: %v", err)
	}
	if err := store.AddEvent(ctx, ticket.ID, "", "admin", adminID, "status", "public", "额度已追加", "done", 4); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Get(ctx, ticket.ID, employee.ID)
	if err != nil || completed.StatusLabel() != "待确认结果" {
		t.Fatalf("result confirmation status=%+v error=%v", completed, err)
	}
	counts, err := store.Counts(ctx, employee.ID)
	if err != nil || counts.Pending != 1 || counts.Finished != 0 {
		t.Fatalf("counts=%+v error=%v", counts, err)
	}
	if rows, err := store.List(ctx, employee.ID, "customer_action", 10, 0); err != nil || len(rows) != 1 {
		t.Fatalf("customer action filter=%v error=%v", rows, err)
	}
	if err := store.AddEvent(ctx, ticket.ID, employee.ID, "customer", employee.ID, "status", "public", "已确认结果", "closed", 5); err != nil {
		t.Fatal(err)
	}
	counts, err = store.Counts(ctx, employee.ID)
	if err != nil || counts.Pending != 0 || counts.Finished != 1 {
		t.Fatalf("confirmed counts=%+v error=%v", counts, err)
	}
}
