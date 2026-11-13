package companyskills

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestDesiredUsesLiveEmployeeAndDepartmentWithoutLeakingOtherSkills(t *testing.T) {
	a, b := testBundle("bippo-renewal", "steps"), testBundle("bippo-finance", "steps")
	c := Catalog{Skills: []Skill{
		{Name: a.Name, Current: 1, TrialEmployee: "alice", Revisions: []Revision{{Bundle: a}}},
		{Name: b.Name, Current: 1, Departments: []string{"finance"}, Revisions: []Revision{{Bundle: b}}},
	}}
	alice := repo.Employee{ID: "alice", WindowsUser: "work1", Department: "sales", Status: repo.StatusActive}
	plan, err := c.Desired(alice)
	if err != nil || len(plan.Skills) != 1 || plan.Skills[0].Name != a.Name {
		t.Fatalf("wrong audience: %+v %v", plan, err)
	}
	alice.Department = "finance"
	if plan, err = c.Desired(alice); err != nil || len(plan.Skills) != 2 {
		t.Fatalf("department change: %+v %v", plan, err)
	}
	alice.Status = repo.StatusOffboarded
	if plan, err = c.Desired(alice); err != nil || plan.User != "" || len(plan.Skills) != 0 {
		t.Fatalf("offboarded: %+v %v", plan, err)
	}
}

func testStore(t *testing.T) (*dbstore.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN is not set")
	}
	ctx := context.Background()
	dsn, err := dbstore.TestDatabaseDSN(ctx, dsn, "aienv_test_companyskills")
	if err != nil {
		t.Fatal(err)
	}
	db, err := dbstore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Pool().Exec(ctx, `drop schema public cascade; create schema public; grant all on schema public to public`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return dbstore.NewStore(db), ctx
}

type failedAudit struct{ repo.Audit }

func (failedAudit) Append(context.Context, repo.AuditEvent) (string, error) {
	return "", errors.New("archive unavailable")
}

type failedStore struct{ repo.Store }

func (s failedStore) InTx(ctx context.Context, fn func(repo.Store) error) error {
	return s.Store.InTx(ctx, func(tx repo.Store) error { return fn(failedTx{tx}) })
}

type failedTx struct{ repo.Store }

func (s failedTx) Audit() repo.Audit { return failedAudit{s.Store.Audit()} }

func TestCatalogLifecycleCASAndAtomicAudit(t *testing.T) {
	store, ctx := testStore(t)
	alice, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1", Department: "sales"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work2", Department: "finance"})
	if err != nil {
		t.Fatal(err)
	}
	change := Change{Action: "save", Name: "bippo-renewal", Title: "续保清单", Bundle: testBundle("bippo-renewal", "first"), TrialEmployee: alice.ID, ActorType: "customer", ActorID: "customer-manager"}
	if err := Apply(ctx, failedStore{store}, change); err == nil {
		t.Fatal("audit failure accepted")
	}
	if c, v, err := Load(ctx, store.Settings()); err != nil || v != 0 || len(c.Skills) != 0 {
		t.Fatalf("failed transaction persisted: %+v %d %v", c, v, err)
	}
	apply := func() {
		t.Helper()
		if err := Apply(ctx, store, change); err != nil {
			t.Fatal(err)
		}
		change.ExpectedVersion++
	}
	apply()
	if err := Apply(ctx, store, Change{ExpectedVersion: 0, Action: "revoke", Name: change.Name, ActorType: "admin", ActorID: "ops"}); !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	c, _, _ := Load(ctx, store.Settings())
	plan, _ := c.Desired(alice)
	other, _ := c.Desired(bob)
	if len(plan.Skills) != 1 || len(other.Skills) != 0 {
		t.Fatal("trial leaked")
	}
	change.Action = "assign"
	change.Employees = []string{alice.ID}
	change.Departments = []string{"finance"}
	apply()
	c, _, _ = Load(ctx, store.Settings())
	other, _ = c.Desired(bob)
	if len(other.Skills) != 1 {
		t.Fatal("department assignment missing")
	}
	change.Action = "save"
	change.Bundle = testBundle(change.Name, "second")
	apply()
	c, _, _ = Load(ctx, store.Settings())
	plan, _ = c.Desired(alice)
	if plan.Skills[0].Version != 2 {
		t.Fatal("new revision not delivered")
	}
	change.Action = "rollback"
	change.Revision = 1
	apply()
	c, _, _ = Load(ctx, store.Settings())
	plan, _ = c.Desired(alice)
	if plan.Skills[0].Version != 1 {
		t.Fatal("rollback did not use history")
	}
	change.Action = "revoke"
	apply()
	c, _, _ = Load(ctx, store.Settings())
	plan, _ = c.Desired(alice)
	other, _ = c.Desired(bob)
	if len(plan.Skills) != 0 || len(other.Skills) != 0 || len(c.Skills[0].Revisions) != 2 {
		t.Fatal("revoke lost history or kept audience")
	}
	events, _, err := store.Audit().Search(ctx, repo.AuditFilter{TargetType: "company_skill", Limit: 20})
	if err != nil || len(events) != 5 {
		t.Fatalf("audit: %d %v", len(events), err)
	}
	for _, ev := range events {
		if ev.ActorType != "customer" || ev.ActorID != "customer-manager" {
			t.Fatal("lost customer identity")
		}
	}
}
