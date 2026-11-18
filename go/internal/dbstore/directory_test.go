package dbstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

func TestAnOpsAccountWaitsUntilSomebodyOpensIt(t *testing.T) {
	s, ctx := newTestStore(t)
	d := s.db.Directory()
	wei := dirAccount(51, directory.RoleOps, directory.StatusActive)
	wei.Username = "wei.peng@teenet.io"
	readDirectory(t, ctx, d, wei, dirAccount(52, directory.RoleEmployee, directory.StatusActive))
	stranger := dirAccount(53, directory.RoleOps, directory.StatusActive)

	if d.IsBound(ctx, wei.ID) {
		t.Fatal("an ops account nobody opened reads as bound")
	}
	for _, a := range []directory.Account{wei, wei, stranger} {
		if err := d.RecordOpsRequest(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := d.PendingOpsAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PendingOps{}
	for _, p := range pending {
		got[p.Username] = p
	}
	if len(pending) != 2 || got[wei.Username].Attempts != 2 || !got[wei.Username].Known || got[stranger.Username].Known {
		t.Fatalf("pending = %+v", pending)
	}

	// Somebody not in the copy of the list yet cannot be opened.
	if _, err := d.OpenOpsAccount(ctx, stranger.ID, "stranger", repo.RoleAdmin); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("opened an account the list does not have: %v", err)
	}
	if _, err := d.OpenOpsAccount(ctx, dirAccount(52, "", "").ID, "u52", repo.RoleAdmin); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("opened an employee: %v", err)
	}
	if _, err := d.OpenOpsAccount(ctx, wei.ID, " Wei.Peng ", repo.RoleViewer); err != nil {
		t.Fatalf("open: %v", err)
	}
	user, err := d.AdminForSignIn(ctx, wei.ID)
	if err != nil || user != "wei.peng" {
		t.Fatalf("after opening, sign-in finds %q, %v", user, err)
	}
	var hash *string
	var role string
	if err := s.db.Pool().QueryRow(ctx, `select password_hash, role from admin_principals where username = 'wei.peng'`).Scan(&hash, &role); err != nil {
		t.Fatal(err)
	}
	if hash != nil || role != repo.RoleViewer {
		t.Errorf("opened account: password %v, role %q", hash, role)
	}
	if _, err := d.OpenOpsAccount(ctx, wei.ID, "wei2", repo.RoleAdmin); !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("opened the same tool account twice: %v", err)
	}
	pending, _ = d.PendingOpsAccounts(ctx)
	if len(pending) != 1 || pending[0].ID != stranger.ID {
		t.Errorf("after opening, pending = %+v", pending)
	}
}

// An AI workspace account is opened for somebody the list has: active, not
// ops, and without a live account here.
func TestOnlyPeopleWithoutAnAccountCanGetOne(t *testing.T) {
	s, ctx := newTestStore(t)
	d := s.db.Directory()
	boss, amy, bob := dirAccount(1, directory.RoleAdmin, directory.StatusActive), dirAccount(2, directory.RoleEmployee, directory.StatusActive), dirAccount(3, directory.RoleEmployee, directory.StatusActive)
	ops, gone := dirAccount(4, directory.RoleOps, directory.StatusActive), dirAccount(5, directory.RoleEmployee, directory.StatusDisabled)
	readDirectory(t, ctx, d, boss, amy, bob, ops, gone)

	ids := func() []string {
		t.Helper()
		people, err := d.WithoutWorkspace(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range people {
			out = append(out, p.Username)
		}
		return out
	}
	if got := fmt.Sprint(ids()); got != "[u1 u2 u3]" {
		t.Fatalf("candidates = %s", got)
	}

	emp, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work2", Name: amy.Name, DirectoryID: amy.ID})
	if err != nil || emp.DirectoryID != amy.ID {
		t.Fatalf("create linked: %+v %v", emp, err)
	}
	if got := fmt.Sprint(ids()); got != "[u1 u3]" {
		t.Errorf("after amy got one: %s", got)
	}
	if _, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work9", DirectoryID: amy.ID}); !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("a second live account for amy: %v", err)
	}

	// Somebody whose account was closed still has it.
	if _, err := s.Employees().Offboard(ctx, emp.ID, emp.Version); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(ids()); got != "[u1 u3]" {
		t.Errorf("after amy left: %s", got)
	}
}

// A rename in the tool renames the AI workspace account; what was recorded
// under the old name -- a machine bound, a day of usage -- keeps it.
func TestARenameReachesTheAccountButNotTheRecords(t *testing.T) {
	s, ctx := newTestStore(t)
	d := s.db.Directory()
	amy := dirAccount(2, directory.RoleEmployee, directory.StatusActive)
	readDirectory(t, ctx, d, amy)
	emp, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work2", Name: amy.Name, DirectoryID: amy.ID})
	if err != nil {
		t.Fatal(err)
	}
	dev, _ := s.Devices().EnsureByHostname(ctx, "desktop-02")
	if _, err := s.Bindings().Bind(ctx, dev.ID, emp.ID, "", "admin"); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	usage := []repo.UsageRow{{EmployeeID: emp.ID, KeyAlias: "emp-work2", ModelGroup: "sonnet", Calls: 1, CostUSD: "1"}}
	if err := s.Usage().UpsertDay(ctx, day, usage); err != nil {
		t.Fatal(err)
	}

	amy.Name = "Amy Chan"
	readDirectory(t, ctx, d, amy)
	after, err := s.Employees().ByID(ctx, emp.ID)
	if err != nil || after.Name != "Amy Chan" {
		t.Fatalf("account after rename: %+v %v", after, err)
	}
	// The day is counted again after the rename, as the snapshot does.
	if err := s.Usage().UpsertDay(ctx, day, usage); err != nil {
		t.Fatal(err)
	}
	if b, err := s.Bindings().Open(ctx, dev.ID); err != nil || b.EmployeeName != "User 2" {
		t.Errorf("binding name = %q %v", b.EmployeeName, err)
	}
	names, err := s.Usage().NamesInPeriod(ctx, repo.UsageFilter{From: day, To: day.AddDate(0, 0, 1)})
	if err != nil || names[emp.ID] != "User 2" {
		t.Errorf("usage names = %v %v", names, err)
	}
	// A new day is counted under the new name.
	if err := s.Usage().UpsertDay(ctx, day.AddDate(0, 0, 1), usage); err != nil {
		t.Fatal(err)
	}
	if names, _ := s.Usage().NamesInPeriod(ctx, repo.UsageFilter{From: day, To: day.AddDate(0, 0, 2)}); names[emp.ID] != "Amy Chan" {
		t.Errorf("latest name in the period = %q", names[emp.ID])
	}
}

func TestAViewOnlyAdminIsAViewerInTheTicketSystem(t *testing.T) {
	s, ctx := newTestStore(t)
	db := s.db
	d := db.Directory()
	tickets := db.Tickets()

	viewer, staff := dirAccount(1, directory.RoleAdmin, directory.StatusActive), dirAccount(2, directory.RoleEmployee, directory.StatusActive)
	viewer.TicketRole = directory.TicketView
	readDirectory(t, ctx, d, viewer, staff)
	v, ok := contactFor(t, ctx, db, viewer.ID)
	if !ok || v.Role != "viewer" {
		t.Fatalf("view-only admin's contact = %+v", v)
	}

	// The viewer opens a colleague's ticket but cannot approve what it costs.
	e, _ := contactFor(t, ctx, db, staff.ID)
	ticket, err := tickets.Create(ctx, e.ID, "viewer-key", "general", "額度用完，申請增加", "申請追加額度", "normal", []byte(`{"schema_version":4,"request_kind":"quota"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.GetForContact(ctx, ticket.ID, v.ID, v.Role); err != nil {
		t.Errorf("a viewer could not read a colleague's ticket: %v", err)
	}
	var adminID string
	if err := db.Pool().QueryRow(ctx, `insert into admin_principals(username,password_hash,role)
		values('viewer_operator','hash','admin') returning id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if err := tickets.AddEvent(ctx, ticket.ID, "", "admin", adminID, "status", "public", "開始核對", "working", 1); err != nil {
		t.Fatal(err)
	}
	payload := map[string]string{"amount": "100", "unit": "tokens", "valid_until": "2026-12-31", "cost": "1", "currency": "USD"}
	if err := tickets.SaveProposal(ctx, ticket.ID, adminID, 2, "quota", payload, true); err != nil {
		t.Fatal(err)
	}
	proposal, err := tickets.LatestProposal(ctx, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tickets.DecideProposal(ctx, ticket.ID, v.ID, 3, proposal.ID, "approved"); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("a viewer approved a paid proposal: %v", err)
	}

	// Allowed to approve in the tool: a manager from the next reading.
	viewer.TicketRole = directory.TicketApprove
	plan, _ := readDirectory(t, ctx, d, viewer, staff)
	if len(plan.Changes) != 1 || plan.Changes[0].Kind != directory.ChangeRole {
		t.Errorf("changes = %+v", plan.Changes)
	}
	if v, _ := contactFor(t, ctx, db, viewer.ID); v.Role != "manager" {
		t.Errorf("after approve: %+v", v)
	}
	// Nothing changed: nothing to report.
	if plan, _ := readDirectory(t, ctx, d, viewer, staff); len(plan.Changes) != 0 {
		t.Errorf("a quiet reading reported %+v", plan.Changes)
	}

	// Somebody new since the last reading signs in as a viewer on the spot.
	fresh := dirAccount(3, directory.RoleAdmin, directory.StatusActive)
	fresh.TicketRole = directory.TicketView
	id, err := d.CustomerForSignIn(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := contactFor(t, ctx, db, fresh.ID); c.ID != id || c.Role != "viewer" {
		t.Errorf("signed-in newcomer = %+v", c)
	}
}
