package dbstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// newTestStore gives a migrated, empty database and a Store on it.
func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(database), ctx
}

func mustCreate(t *testing.T, ctx context.Context, s *Store, user string) repo.Employee {
	t.Helper()
	e, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: user, Name: user})
	if err != nil {
		t.Fatalf("create %s: %v", user, err)
	}
	return e
}

func TestCreateAndReadEmployee(t *testing.T) {
	s, ctx := newTestStore(t)

	created, err := s.Employees().Create(ctx, repo.NewEmployee{
		WindowsUser: "Work1", Name: "张三", Department: "研发",
		CodexAccount: "work1@example.com", ExternalID: "E-0042",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Error("created employee has no id")
	}
	// Windows account names are case-insensitive; storing "Work1" as typed
	// would let "work1" be onboarded a second time, with a second set of
	// credentials in a second directory.
	if created.WindowsUser != "work1" {
		t.Errorf("windows user = %q, want it lower-cased", created.WindowsUser)
	}
	if !created.Active() || created.AuthEpoch != 1 || created.Version != 1 {
		t.Errorf("new employee = %+v, want active at epoch 1 version 1", created)
	}

	for _, lookup := range []string{"work1", "WORK1", " Work1 "} {
		got, err := s.Employees().ByWindowsUser(ctx, lookup)
		if err != nil {
			t.Fatalf("ByWindowsUser(%q): %v", lookup, err)
		}
		if got.ID != created.ID {
			t.Errorf("ByWindowsUser(%q) found %s, want %s", lookup, got.ID, created.ID)
		}
	}
	byID, err := s.Employees().ByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if byID.Name != "张三" || byID.Department != "研发" || byID.ExternalID != "E-0042" {
		t.Errorf("ByID = %+v, want the fields as created", byID)
	}
}

func TestCreateRejectsDuplicates(t *testing.T) {
	s, ctx := newTestStore(t)
	first := mustCreate(t, ctx, s, "work1")

	_, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "WORK1"})
	if !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("creating the same Windows user again: error = %v, want ErrDuplicate", err)
	}

	// An offboarded person still holds their name. Reopening the row is the
	// intended move; a second row would split their history in two.
	if _, err := s.Employees().Offboard(ctx, first.ID, first.Version); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if _, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"}); !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("creating over an offboarded row: error = %v, want ErrDuplicate", err)
	}

	if _, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work2", ExternalID: "E-1"}); err != nil {
		t.Fatalf("create work2: %v", err)
	}
	if _, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work3", ExternalID: "E-1"}); !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("reusing an external id: error = %v, want ErrDuplicate", err)
	}
	// But "no employee number" is not a value, and several people may have it.
	if _, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work4"}); err != nil {
		t.Errorf("a second employee without an external id was rejected: %v", err)
	}
	if _, err := s.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "   "}); err == nil {
		t.Error("an employee with a blank Windows user was accepted")
	}
}

func TestListLeavesOutPeopleWhoHaveGone(t *testing.T) {
	s, ctx := newTestStore(t)
	stays := mustCreate(t, ctx, s, "work1")
	leaves := mustCreate(t, ctx, s, "work2")
	if _, err := s.Employees().Offboard(ctx, leaves.ID, leaves.Version); err != nil {
		t.Fatalf("offboard: %v", err)
	}

	active, err := s.Employees().List(ctx, repo.EmployeeFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 1 || active[0].ID != stays.ID {
		t.Fatalf("active list = %+v, want only work1", active)
	}

	all, err := s.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("full list has %d rows, want 2", len(all))
	}
	if all[0].WindowsUser != "work1" || all[1].WindowsUser != "work2" {
		t.Errorf("list is not ordered by windows user: %q, %q", all[0].WindowsUser, all[1].WindowsUser)
	}
}

func TestUpdateProfileRefusesAStaleVersion(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")

	updated, err := s.Employees().UpdateProfile(ctx, e.ID, e.Version, repo.Profile{
		Name: "李四", Department: "财务", CodexAccount: "l@example.com",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != e.Version+1 || updated.Name != "李四" {
		t.Fatalf("updated = %+v, want the new name at version %d", updated, e.Version+1)
	}

	// Two administrators on the same page is an ordinary Tuesday. The second
	// save must be refused, not silently applied over the first.
	_, err = s.Employees().UpdateProfile(ctx, e.ID, e.Version, repo.Profile{Name: "王五"})
	if !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("stale update: error = %v, want ErrConflict", err)
	}
	after, err := s.Employees().ByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.Name != "李四" {
		t.Errorf("name = %q; the refused save changed the row anyway", after.Name)
	}
}

func TestOffboardRaisesTheEpochAndIsIdempotent(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")

	gone, err := s.Employees().Offboard(ctx, e.ID, e.Version)
	if err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if gone.Active() {
		t.Error("employee is still active after offboarding")
	}
	if gone.OffboardedAt == nil {
		t.Error("offboarded_at was not set")
	}
	// The epoch is what makes it stick: a provisioning task already in flight
	// comes back for an epoch that no longer exists and is dropped.
	if gone.AuthEpoch != e.AuthEpoch+1 {
		t.Errorf("auth epoch = %d, want %d", gone.AuthEpoch, e.AuthEpoch+1)
	}

	// The console offers "retry offboard" after a partial failure; that retry
	// must not fail on the step that did succeed.
	again, err := s.Employees().Offboard(ctx, gone.ID, gone.Version)
	if err != nil {
		t.Fatalf("second offboard: %v", err)
	}
	if again.AuthEpoch != gone.AuthEpoch || again.Version != gone.Version {
		t.Errorf("the second offboard changed the row: %+v then %+v", gone, again)
	}

	back, err := s.Employees().Reopen(ctx, again.ID, again.Version)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !back.Active() || back.OffboardedAt != nil {
		t.Errorf("reopened employee = %+v, want active with no offboard date", back)
	}
	// Not back to the old epoch: whatever was issued before the departure
	// stays dead.
	if back.AuthEpoch != again.AuthEpoch+1 {
		t.Errorf("auth epoch after reopen = %d, want %d", back.AuthEpoch, again.AuthEpoch+1)
	}

	reissued, err := s.Employees().BumpAuthEpoch(ctx, back.ID, back.Version)
	if err != nil {
		t.Fatalf("bump epoch: %v", err)
	}
	if reissued.AuthEpoch != back.AuthEpoch+1 || !reissued.Active() {
		t.Errorf("re-issue = %+v, want an active employee one epoch on", reissued)
	}
}

func TestMissingEmployeeIsNotFound(t *testing.T) {
	s, ctx := newTestStore(t)
	for _, id := range []string{
		"6f1e9e6c-0000-4000-8000-000000000000", // well-formed, absent
		"not-a-uuid",                           // from a hand-edited URL
	} {
		if _, err := s.Employees().ByID(ctx, id); !errors.Is(err, repo.ErrNotFound) {
			t.Errorf("ByID(%q): error = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := s.Employees().ByWindowsUser(ctx, "nobody"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("ByWindowsUser: error = %v, want ErrNotFound", err)
	}
}

func TestSetModelsReplacesTheWholeSet(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")

	if err := s.Employees().SetModels(ctx, e.ID, []string{"claude-4.5-sonnet", "gemini-2.5-pro"}); err != nil {
		t.Fatalf("set models: %v", err)
	}
	models, err := s.Employees().Models(ctx, e.ID)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 2 || models[0] != "claude-4.5-sonnet" || models[1] != "gemini-2.5-pro" {
		t.Fatalf("models = %v", models)
	}

	// Replace, not merge: taking a model away has to actually take it away.
	if err := s.Employees().SetModels(ctx, e.ID, []string{"gemini-2.5-pro", "gemini-2.5-pro"}); err != nil {
		t.Fatalf("set models again: %v", err)
	}
	models, err = s.Employees().Models(ctx, e.ID)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 1 || models[0] != "gemini-2.5-pro" {
		t.Fatalf("models = %v, want just gemini-2.5-pro", models)
	}

	if err := s.Employees().SetModels(ctx, e.ID, nil); err != nil {
		t.Fatalf("clear models: %v", err)
	}
	models, err = s.Employees().Models(ctx, e.ID)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %v, want none", models)
	}

	// With an empty list there is nothing to insert and so no foreign key to
	// violate; without the explicit check this would report success for an
	// employee who does not exist.
	err = s.Employees().SetModels(ctx, "6f1e9e6c-0000-4000-8000-000000000000", nil)
	if !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("set models for a missing employee: error = %v, want ErrNotFound", err)
	}
}

func TestInTxRollsBackEverything(t *testing.T) {
	s, ctx := newTestStore(t)

	wantErr := errors.New("the gateway said no")
	err := s.InTx(ctx, func(tx repo.Store) error {
		e, err := tx.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
		if err != nil {
			return err
		}
		if _, err := tx.Quotas().Set(ctx, e.ID, repo.Quota{
			MonthlyBudget: "50", RPM: 60, TPM: 2000000, Parallel: 8,
		}, 0); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("InTx error = %v, want the caller's error", err)
	}

	// Onboarding writes the roster row, the quota and the tasks together. A
	// failure part way must leave none of it, or the next run finds an
	// employee with no quota and no way to tell that it is half done.
	list, err := s.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("the rolled-back transaction left %d employees behind", len(list))
	}
}

func TestInTxRefusesToNest(t *testing.T) {
	s, ctx := newTestStore(t)
	err := s.InTx(ctx, func(tx repo.Store) error {
		// A caller that believes it opened a transaction, and did not, gets a
		// partial write on the first error. Saying so is the whole point.
		return tx.InTx(ctx, func(repo.Store) error { return nil })
	})
	if err == nil {
		t.Fatal("nested InTx was allowed")
	}
	if !strings.Contains(err.Error(), "transaction") {
		t.Errorf("nested InTx error = %v, want it to say what went wrong", err)
	}
}

func TestDeleteHidesTheAccountAndFreesTheName(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work9")

	if _, err := s.Employees().Delete(ctx, e.ID, e.Version); err == nil {
		t.Fatal("an open account must not be deletable; it has to be closed first")
	}
	gone, err := s.Employees().Offboard(ctx, e.ID, e.Version)
	if err != nil {
		t.Fatalf("offboard: %v", err)
	}
	deleted, err := s.Employees().Delete(ctx, gone.ID, gone.Version)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleted.Deleted() || deleted.Status != repo.StatusOffboarded {
		t.Fatalf("after delete: %+v", deleted)
	}
	// Deleting again is not an error: the console offers a retry.
	if again, err := s.Employees().Delete(ctx, deleted.ID, deleted.Version); err != nil || !again.Deleted() {
		t.Fatalf("second delete: %+v, %v", again, err)
	}

	// Gone from every list but the one that asks for it.
	if all, _ := s.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true}); len(all) != 0 {
		t.Fatalf("the full list still shows the deleted account: %+v", all)
	}
	withDeleted, _ := s.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true, IncludeDeleted: true})
	if len(withDeleted) != 1 || !withDeleted[0].Deleted() {
		t.Fatalf("IncludeDeleted list = %+v", withDeleted)
	}
	if _, err := s.Employees().ByWindowsUser(ctx, "work9"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("ByWindowsUser must not find a deleted account: %v", err)
	}
	if _, err := s.Employees().ByID(ctx, e.ID); err != nil {
		t.Fatalf("ByID must still resolve for history: %v", err)
	}

	// The name is free again, and the new person is a new row.
	fresh := mustCreate(t, ctx, s, "work9")
	if fresh.ID == e.ID || fresh.Deleted() {
		t.Fatalf("re-creating the name must make a new account: %+v", fresh)
	}
	if found, err := s.Employees().ByWindowsUser(ctx, "work9"); err != nil || found.ID != fresh.ID {
		t.Fatalf("ByWindowsUser after reuse = %+v, %v", found, err)
	}
}

func TestSyncNowLeavesANonceOnTheDevice(t *testing.T) {
	s, ctx := newTestStore(t)
	d, _ := s.Devices().EnsureByHostname(ctx, "PC-9")
	if _, err := s.Devices().RequestSync(ctx, d.ID, ""); err == nil {
		t.Fatal("a nonce is required")
	}
	got, err := s.Devices().RequestSync(ctx, d.ID, "n1")
	if err != nil || got.SyncNonce != "n1" {
		t.Fatalf("RequestSync = %+v, %v", got, err)
	}
	if again, _ := s.Devices().ByID(ctx, d.ID); again.SyncNonce != "n1" {
		t.Fatalf("nonce not stored: %+v", again)
	}
	s.Devices().Revoke(ctx, d.ID)
	if _, err := s.Devices().RequestSync(ctx, d.ID, "n2"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("a revoked machine cannot be asked to sync: %v", err)
	}
}
