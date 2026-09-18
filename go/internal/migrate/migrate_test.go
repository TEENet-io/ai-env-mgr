package migrate

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// fakeOSS is a bucket with whatever the test puts in it.
type fakeOSS struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeOSS() *fakeOSS { return &fakeOSS{objects: map[string][]byte{}} }

func (f *fakeOSS) Get(key string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, "", ossclient.ErrNotFound
	}
	return data, "etag", nil
}

func (f *fakeOSS) List(prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	return out, nil
}

func (f *fakeOSS) put(t *testing.T, key string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
}

func newStore(t *testing.T) (*dbstore.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN is not set; skipping the PostgreSQL integration tests")
	}
	ctx := context.Background()
	dsn, err := dbstore.TestDatabaseDSN(ctx, dsn, "aienv_test_migrate")
	if err != nil {
		t.Fatalf("test database: %v", err)
	}
	database, err := dbstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(database.Close)
	if _, err := database.Pool().Exec(ctx, `drop schema public cascade; create schema public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := database.Pool().Exec(ctx, `grant all on schema public to public`); err != nil {
		t.Fatalf("restore schema grant: %v", err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return dbstore.NewStore(database), ctx
}

// liveBucket is a bucket shaped like the one in production: a roster, a
// policy, and two bound machines.
func liveBucket(t *testing.T) *fakeOSS {
	t.Helper()
	objects := newFakeOSS()
	objects.put(t, ossclient.AdminKey("users.json"), model.Users{
		Users: []model.UserEntry{
			{WindowsUser: "work1", Name: "张三", Department: "研发", Enabled: true,
				CodexAccount: "work1@example.com"},
			{WindowsUser: "Work2", Name: "李四", Enabled: true},
			{WindowsUser: "gone", Name: "王五", Enabled: false},
		},
	})
	objects.put(t, ossclient.PolicyKey(), model.Policy{
		BlockEnabled: true, BlockedDomains: []string{"openai.com"},
		SyncIntervalMinutes: 30, UpdatedAt: "2026-09-01T00:00:00Z",
	})
	objects.put(t, ossclient.BindingKey("DESKTOP-01"), model.Binding{
		User: "work1", BoundAt: "2026-09-01T00:00:00Z", Note: "第一台",
	})
	objects.put(t, ossclient.BindingKey("DESKTOP-02"), model.Binding{
		User: "Work2", BoundAt: "2026-09-02T00:00:00Z",
	})
	objects.objects[ossclient.UserKey("work1", "credentials.zip")] = []byte("PK-not-a-real-zip")
	return objects
}

func TestImportBringsTheLiveStateIn(t *testing.T) {
	store, ctx := newStore(t)
	objects := liveBucket(t)

	im := &Importer{Store: store, Objects: objects, Actor: "migration"}
	report, err := im.Run(ctx)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.Employees != 3 || report.Devices != 2 || report.Bindings != 2 || !report.PolicyFound {
		t.Fatalf("report = %s", report)
	}

	all, err := store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("imported %d employees", len(all))
	}
	work2, err := store.Employees().ByWindowsUser(ctx, "work2")
	if err != nil {
		t.Fatalf("work2: %v", err)
	}
	// The roster spelled it "Work2"; Windows does not care and neither does
	// the database, but the old name has to stay findable.
	if work2.Name != "李四" {
		t.Errorf("work2 = %+v", work2)
	}
	id, err := store.LegacyIDs().Lookup(ctx, repo.LegacyEmployee, "Work2")
	if err != nil {
		t.Fatalf("legacy lookup: %v", err)
	}
	if id != work2.ID {
		t.Errorf("the old name maps to %s, want %s", id, work2.ID)
	}

	gone, err := store.Employees().ByWindowsUser(ctx, "gone")
	if err != nil {
		t.Fatalf("gone: %v", err)
	}
	if gone.Active() {
		t.Error("a disabled roster entry was imported as active")
	}

	// The token is not invented. It cannot be: the gateway returns a token
	// once, and the one on the employee's machine is unreadable from here.
	if _, err := store.Credentials().Live(ctx, all[0].ID, repo.PurposeCodexGateway); err == nil {
		t.Error("the import invented a credential")
	}

	current, err := store.Policies().Current(ctx)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	var policy model.Policy
	if err := json.Unmarshal(current.Content, &policy); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if !policy.BlockEnabled || policy.SyncIntervalMinutes != 30 {
		t.Errorf("imported policy = %+v", policy)
	}
}

func TestImportingTwiceChangesNothing(t *testing.T) {
	store, ctx := newStore(t)
	objects := liveBucket(t)
	im := &Importer{Store: store, Objects: objects, Actor: "migration"}

	if _, err := im.Run(ctx); err != nil {
		t.Fatalf("first import: %v", err)
	}
	before, err := store.Employees().ByWindowsUser(ctx, "work1")
	if err != nil {
		t.Fatalf("work1: %v", err)
	}

	second, err := im.Run(ctx)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Employees != 0 || second.Bindings != 0 {
		t.Errorf("the second import created rows: %s", second)
	}

	after, err := store.Employees().ByWindowsUser(ctx, "work1")
	if err != nil {
		t.Fatalf("work1: %v", err)
	}
	// The epoch must not move. Raising it would invalidate the token the
	// employee is using right now, during a migration nobody asked for.
	if after.AuthEpoch != before.AuthEpoch {
		t.Errorf("the epoch moved on a re-import: %d then %d", before.AuthEpoch, after.AuthEpoch)
	}
	versions, err := store.Policies().List(ctx, 0)
	if err != nil {
		t.Fatalf("policies: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("%d policy versions after two imports; the history would read as a change", len(versions))
	}
}

func TestImportSurfacesWhatItCannotResolve(t *testing.T) {
	store, ctx := newStore(t)
	objects := liveBucket(t)
	// A machine bound to somebody who is not on the roster. Inventing an
	// employee for them would bury exactly the thing worth finding.
	objects.put(t, ossclient.BindingKey("DESKTOP-09"), model.Binding{User: "contractor"})

	im := &Importer{Store: store, Objects: objects, Actor: "migration"}
	report, err := im.Run(ctx)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "contractor") {
		t.Fatalf("warnings = %v, want the unknown user named", report.Warnings)
	}
	if _, err := store.Employees().ByWindowsUser(ctx, "contractor"); err == nil {
		t.Error("the import created an employee that was not on the roster")
	}
	// The machine itself is still imported: it exists, and a console that
	// cannot see it cannot fix it.
	if _, err := store.Devices().ByHostname(ctx, "DESKTOP-09"); err != nil {
		t.Errorf("the machine was not imported: %v", err)
	}
}

func TestAHalfImportLeavesNothing(t *testing.T) {
	store, ctx := newStore(t)
	objects := liveBucket(t)
	// A roster entry the database will refuse, after the earlier ones have
	// already been written inside the transaction.
	objects.put(t, ossclient.AdminKey("users.json"), model.Users{
		Users: []model.UserEntry{
			{WindowsUser: "work1", Enabled: true},
			{WindowsUser: "   ", Enabled: true},
		},
	})

	im := &Importer{Store: store, Objects: objects, Actor: "migration"}
	if _, err := im.Run(ctx); err == nil {
		t.Fatal("an import with a broken roster entry reported success")
	}
	all, err := store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// A half-imported database is worse than an empty one, because it looks
	// ready.
	if len(all) != 0 {
		t.Errorf("the failed import left %d employees behind", len(all))
	}
}

func TestTheComparisonIsCleanAfterAnImport(t *testing.T) {
	store, ctx := newStore(t)
	objects := liveBucket(t)
	im := &Importer{Store: store, Objects: objects, Actor: "migration"}
	if _, err := im.Run(ctx); err != nil {
		t.Fatalf("import: %v", err)
	}

	cmp := &Comparer{Store: store, Objects: objects}
	result, err := cmp.Run(ctx)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if result.Checked == 0 {
		t.Fatal("the comparison checked nothing")
	}

	// The one difference expected during a migration, and the one that decides
	// when it is safe to switch: work1 is using a token the database does not
	// hold, so an export would withdraw their credentials.
	if len(result.Differences) != 1 {
		t.Fatalf("differences = %v, want only the credential that has to be re-issued", result.Differences)
	}
	if !strings.Contains(result.Differences[0].What, "re-issue") {
		t.Errorf("difference = %q, want it to say what to do", result.Differences[0].What)
	}
	if result.Clean() {
		t.Error("Clean() is true while a difference is reported")
	}
}

func TestTheComparisonNamesWhatWouldChange(t *testing.T) {
	store, ctx := newStore(t)
	objects := liveBucket(t)
	im := &Importer{Store: store, Objects: objects, Actor: "migration"}
	if _, err := im.Run(ctx); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Somebody edits the live policy object behind the console's back -- or,
	// more usefully, the old console is still running and published one.
	objects.put(t, ossclient.PolicyKey(), model.Policy{
		BlockEnabled: true, BlockedDomains: []string{"openai.com"},
		SyncIntervalMinutes: 5, UpdatedAt: "2026-09-18T00:00:00Z",
	})
	// And a machine changes hands in the published world only.
	objects.put(t, ossclient.BindingKey("DESKTOP-01"), model.Binding{User: "work2"})

	cmp := &Comparer{Store: store, Objects: objects}
	result, err := cmp.Run(ctx)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}

	var sawPolicy, sawBinding bool
	for _, d := range result.Differences {
		if d.Object == ossclient.PolicyKey() {
			sawPolicy = true
			// The report has to say what changed, not that something did.
			if !strings.Contains(d.What, "syncIntervalMinutes") {
				t.Errorf("policy difference = %q, want the field named", d.What)
			}
		}
		if d.Object == ossclient.BindingKey("DESKTOP-01") {
			sawBinding = true
			if !strings.Contains(d.What, "work2") || !strings.Contains(d.What, "work1") {
				t.Errorf("binding difference = %q, want both names", d.What)
			}
		}
	}
	if !sawPolicy {
		t.Error("the policy change was not reported")
	}
	if !sawBinding {
		t.Error("the binding change was not reported")
	}
	if strings.Count(result.String(), "\n") == 0 {
		t.Error("the report does not list the differences")
	}
}

// A machine bound in the database but not published is the other direction:
// switching over would hand somebody a machine they do not have today.
func TestTheComparisonNoticesABindingThatOnlyExistsInTheDatabase(t *testing.T) {
	store, ctx := newStore(t)
	objects := liveBucket(t)
	im := &Importer{Store: store, Objects: objects, Actor: "migration"}
	if _, err := im.Run(ctx); err != nil {
		t.Fatalf("import: %v", err)
	}

	delete(objects.objects, ossclient.BindingKey("DESKTOP-02"))
	cmp := &Comparer{Store: store, Objects: objects}
	result, err := cmp.Run(ctx)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	var found bool
	for _, d := range result.Differences {
		if strings.Contains(d.Object, "DESKTOP-02") && strings.Contains(d.What, "not published") {
			found = true
		}
	}
	if !found {
		t.Errorf("differences = %v, want the unpublished binding", result.Differences)
	}
}
