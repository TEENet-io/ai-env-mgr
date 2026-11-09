package dbstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/directory"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func dirAccount(n int, role, status string) directory.Account {
	return directory.Account{
		ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), Username: fmt.Sprintf("u%d", n),
		Name: fmt.Sprintf("User %d", n), Email: fmt.Sprintf("u%d@example.com", n), Role: role, Status: status,
	}
}

// readDirectory reads the list the way the Worker does: compare with the copy, then
// apply.
func readDirectory(t *testing.T, ctx context.Context, d DirectoryStore, accounts ...directory.Account) (directory.Plan, SyncResult) {
	t.Helper()
	list := directory.List{Accounts: accounts}
	if err := list.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	known, err := d.Known(ctx)
	if err != nil {
		t.Fatalf("known: %v", err)
	}
	plan := directory.Compare(known, list)
	res, err := d.Apply(ctx, list, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return plan, res
}

func contactFor(t *testing.T, ctx context.Context, db *DB, id string) (CustomerContact, bool) {
	t.Helper()
	var c CustomerContact
	err := db.pool.QueryRow(ctx, `select id, email, name, coalesce(password_hash, ''), role, disabled_at
		from customer_contacts where directory_id = $1`, id).Scan(&c.ID, &c.Email, &c.Name, &c.PasswordHash, &c.Role, &c.DisabledAt)
	return c, err == nil
}

func openAlerts(t *testing.T, ctx context.Context, s *Store, kind string) []repo.Alert {
	t.Helper()
	all, err := s.Alerts().ListOpen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []repo.Alert
	for _, a := range all {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func TestTheDirectoryGivesCustomersContactsAndTakesThemAway(t *testing.T) {
	s, ctx := newTestStore(t)
	db := s.db
	d := db.Directory()
	tickets := db.Tickets()

	// A contact made by hand before the tool was connected is linked, not
	// duplicated.
	manual, err := tickets.CreateContactWithRole(ctx, "u2@example.com", "Old name", "hash", "employee")
	if err != nil {
		t.Fatal(err)
	}
	ops, admin, staff := dirAccount(1, directory.RoleOps, directory.StatusActive), dirAccount(2, directory.RoleAdmin, directory.StatusActive), dirAccount(3, directory.RoleEmployee, directory.StatusActive)
	_, res := readDirectory(t, ctx, d, ops, admin, staff)
	if res.ContactsCreated != 1 || res.ContactsLinked != 1 {
		t.Fatalf("first reading: %s", res)
	}
	if _, ok := contactFor(t, ctx, db, ops.ID); ok {
		t.Error("an ops account became a customer contact")
	}
	if c, ok := contactFor(t, ctx, db, admin.ID); !ok || c.ID != manual.ID || c.Role != "manager" || c.Name != "User 2" {
		t.Errorf("the hand-made contact was not linked and updated: %+v", c)
	}
	c, ok := contactFor(t, ctx, db, staff.ID)
	if !ok || c.Role != "employee" || c.PasswordHash != "" {
		t.Fatalf("new contact = %+v", c)
	}

	// A session of that contact, and a reading that disables the person.
	if err := tickets.CreateSession(ctx, c.ID, []byte("session-hash-0000000000000000000"), 3600e9, 3600e9); err != nil {
		t.Fatal(err)
	}
	staff.Status = directory.StatusDisabled
	staff.Name = "Renamed"
	_, res = readDirectory(t, ctx, d, ops, admin, staff)
	if res.SessionsEnded != 1 {
		t.Errorf("sessions ended = %d, want the contact's one", res.SessionsEnded)
	}
	if c, _ := contactFor(t, ctx, db, staff.ID); c.DisabledAt == nil || c.Name != "Renamed" {
		t.Errorf("after disabling: %+v", c)
	}

	// Back again: the contact is usable again.
	staff.Status = directory.StatusActive
	readDirectory(t, ctx, d, ops, admin, staff)
	if c, _ := contactFor(t, ctx, db, staff.ID); c.DisabledAt != nil {
		t.Error("re-enabling did not restore the contact")
	}

	// Every change is in the audit trail, made by the sync.
	events, _, err := s.Audit().Search(ctx, repo.AuditFilter{TargetType: "directory_account", TargetID: staff.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, ev := range events {
		got[ev.Action] = ev.ActorID == "directory-sync"
	}
	for _, action := range []string{"directory.added", "directory.renamed", "directory.disabled", "directory.reenabled"} {
		if !got[action] {
			t.Errorf("no %s by the sync in the audit trail: %v", action, got)
		}
	}
}

func TestTheDirectoryRefusesToTakeAccessFromManyAtOnce(t *testing.T) {
	s, ctx := newTestStore(t)
	d := s.db.Directory()
	accounts := []directory.Account{dirAccount(0, directory.RoleAdmin, directory.StatusActive)}
	for i := 1; i <= directory.MaxDisablesPerSync+1; i++ {
		accounts = append(accounts, dirAccount(i, directory.RoleEmployee, directory.StatusActive))
	}
	readDirectory(t, ctx, d, accounts...)
	for i := 1; i < len(accounts); i++ {
		accounts[i].Status = directory.StatusDisabled
	}
	plan, _ := readDirectory(t, ctx, d, accounts...)
	if len(plan.Held) != directory.MaxDisablesPerSync+1 {
		t.Fatalf("held %d", len(plan.Held))
	}
	for _, a := range accounts[1:] {
		if c, _ := contactFor(t, ctx, s.db, a.ID); c.DisabledAt != nil {
			t.Errorf("%s was disabled although the batch was held", a.Username)
		}
	}
	if len(openAlerts(t, ctx, s, repo.AlertDirectoryHeld)) != 1 {
		t.Error("no alert for the held batch")
	}
	// The next reading finds the same batch again: nothing has been stored
	// as disabled behind the alert's back.
	if plan, _ := readDirectory(t, ctx, d, accounts...); len(plan.Held) != directory.MaxDisablesPerSync+1 {
		t.Errorf("the second reading held %d", len(plan.Held))
	}
}

func TestAnAbsentAccountKeepsItsAccessAndRaisesAnAlert(t *testing.T) {
	s, ctx := newTestStore(t)
	d := s.db.Directory()
	a, b := dirAccount(1, directory.RoleAdmin, directory.StatusActive), dirAccount(2, directory.RoleEmployee, directory.StatusActive)
	readDirectory(t, ctx, d, a, b)
	readDirectory(t, ctx, d, a)
	if c, _ := contactFor(t, ctx, s.db, b.ID); c.DisabledAt != nil {
		t.Error("an account missing from the reply was disabled")
	}
	if len(openAlerts(t, ctx, s, repo.AlertDirectoryMissing)) != 1 {
		t.Error("no alert for the missing account")
	}
	readDirectory(t, ctx, d, a, b)
	if len(openAlerts(t, ctx, s, repo.AlertDirectoryMissing)) != 0 {
		t.Error("the missing alert stayed open after the account came back")
	}
}

// A disabled person who still has an AI workspace account is a follow-up
// for an operator; the first version does not close the account itself.
func TestADisabledPersonWithAnAIWorkspaceAccountIsAFollowUp(t *testing.T) {
	s, ctx := newTestStore(t)
	d := s.db.Directory()
	owner, staff := dirAccount(1, directory.RoleAdmin, directory.StatusActive), dirAccount(2, directory.RoleEmployee, directory.StatusActive)
	readDirectory(t, ctx, d, owner, staff)
	emp := mustCreate(t, ctx, s, "work2")
	if _, err := s.db.pool.Exec(ctx, `update employees set directory_id = $1 where id = $2`, staff.ID, emp.ID); err != nil {
		t.Fatal(err)
	}
	staff.Status = directory.StatusDisabled
	readDirectory(t, ctx, d, owner, staff)
	follow := openAlerts(t, ctx, s, repo.AlertDirectoryFollowUp)
	if len(follow) != 1 || follow[0].SubjectID != staff.ID {
		t.Fatalf("follow-ups = %+v", follow)
	}
	after, err := s.Employees().ByID(ctx, emp.ID)
	if err != nil || after.Status != "active" {
		t.Errorf("the AI workspace account was changed by the sync: %+v %v", after, err)
	}
}

func TestAFailingReadingIsRecordedUntilOneSucceeds(t *testing.T) {
	s, ctx := newTestStore(t)
	d := s.db.Directory()
	first, err := d.RecordFailure(ctx, fmt.Errorf("HTTP 502"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := d.RecordFailure(ctx, fmt.Errorf("HTTP 503"))
	if err != nil || !again.Equal(first) {
		t.Errorf("failing since moved: %v -> %v (%v)", first, again, err)
	}
	readDirectory(t, ctx, d, dirAccount(1, directory.RoleAdmin, directory.StatusActive))
	st, err := d.State(ctx)
	if err != nil || st.FailingSince != nil || st.LastError != "" || st.LastSuccessAt == nil {
		t.Errorf("after a success: %+v %v", st, err)
	}
}
