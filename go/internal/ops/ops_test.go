package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// newService gives a migrated, empty database and a Service on it. It skips
// without TEST_PG_DSN, like the other integration tests; see db/README.md.
func newService(t *testing.T) (*Service, *dbstore.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN is not set; skipping the PostgreSQL integration tests")
	}
	ctx := context.Background()
	// A database of this package's own: `go test ./...` runs packages in
	// parallel and each of these suites empties the schema it is about to use.
	dsn, err := dbstore.TestDatabaseDSN(ctx, dsn, "aienv_test_ops")
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
	store := dbstore.NewStore(database)
	return New(store), store, ctx
}

func testQuota() repo.Quota {
	return repo.Quota{MonthlyBudget: "50", RPM: 60, TPM: 2000000, Parallel: 8}
}

func openTasks(t *testing.T, ctx context.Context, store *dbstore.Store) []repo.Task {
	t.Helper()
	tasks, err := store.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	return tasks
}

func kinds(tasks []repo.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, task.Kind)
	}
	return out
}

func TestOnboardWritesEverythingOrNothing(t *testing.T) {
	svc, store, ctx := newService(t)

	employee, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "Work1", Name: "张三", Department: "研发",
		Quota: testQuota(), Models: []string{"claude-4.5-sonnet", "gemini-2.5-pro"},
		Actor: "zhang", RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if employee.WindowsUser != "work1" || !employee.Active() {
		t.Fatalf("employee = %+v", employee)
	}
	// A fresh account still gets its epoch raised: onboarding always issues a
	// token, and the epoch is what the task is aimed at.
	if employee.AuthEpoch != 2 {
		t.Errorf("auth epoch = %d, want 2", employee.AuthEpoch)
	}

	quota, err := store.Quotas().Get(ctx, employee.ID)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if quota.MonthlyBudget != "50.000000" {
		t.Errorf("budget = %q", quota.MonthlyBudget)
	}
	models, err := store.Employees().Models(ctx, employee.ID)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("models = %v", models)
	}

	// The gateway call and the delivery are tasks, not something that happened
	// while the administrator waited.
	tasks := openTasks(t, ctx, store)
	if len(tasks) != 2 {
		t.Fatalf("queued %v, want a provision and an export", kinds(tasks))
	}
	for _, task := range tasks {
		if task.TargetEpoch == nil || *task.TargetEpoch != employee.AuthEpoch {
			t.Errorf("task %s targets epoch %v, want %d", task.Kind, task.TargetEpoch, employee.AuthEpoch)
		}
		if task.EmployeeID != employee.ID {
			t.Errorf("task %s is not linked to the employee", task.Kind)
		}
		// Payloads are read during incidents and pasted into tickets.
		if strings.Contains(string(task.Payload), "sk-") {
			t.Errorf("task payload looks like it carries a secret: %s", task.Payload)
		}
	}

	history, err := store.Audit().ByTarget(ctx, "employee", employee.ID, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(history) != 1 || history[0].Action != ActionOnboard || history[0].ActorID != "zhang" {
		t.Fatalf("audit = %+v, want one onboarding by zhang", history)
	}
	if history[0].RequestID != "req-1" {
		t.Error("the audit row does not carry the request id")
	}
}

func TestOnboardingTwiceRepairsRatherThanDuplicates(t *testing.T) {
	svc, store, ctx := newService(t)
	first, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "work1", Name: "张三", Quota: testQuota(),
		Models: []string{"claude-4.5-sonnet"}, Actor: "zhang",
	})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}

	// The same person again: one row, a new epoch, and the old work replaced
	// rather than left racing against the new.
	second, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "WORK1", Department: "财务", Quota: testQuota(),
		Models: []string{"claude-4.5-sonnet", "gemini-2.5-pro"}, Actor: "li",
	})
	if err != nil {
		t.Fatalf("second onboard: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("a second row was created: %s then %s", first.ID, second.ID)
	}
	if second.AuthEpoch <= first.AuthEpoch {
		t.Errorf("epoch = %d, want it past %d", second.AuthEpoch, first.AuthEpoch)
	}
	// A field the second form left blank keeps what the roster had: reopening
	// from a form nobody re-typed must not blank the notes.
	if second.Name != "张三" || second.Department != "财务" {
		t.Errorf("profile = %q/%q, want the name kept and the department updated", second.Name, second.Department)
	}

	tasks := openTasks(t, ctx, store)
	if len(tasks) != 2 {
		t.Fatalf("open tasks = %v, want only the new provision and export", kinds(tasks))
	}
	for _, task := range tasks {
		if task.TargetEpoch == nil || *task.TargetEpoch != second.AuthEpoch {
			t.Errorf("task %s is still aimed at epoch %v", task.Kind, task.TargetEpoch)
		}
	}
	// Nothing from the first round is runnable any more.
	if _, err := store.Tasks().Claim(ctx, "w", nil, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}
}

func TestOffboardClosesEverythingItCanAndQueuesTheRest(t *testing.T) {
	svc, store, ctx := newService(t)
	employee, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "work1", Quota: testQuota(), Actor: "zhang",
	})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}

	// Stand in for the Worker having done its job: a grant and a credential
	// exist by the time somebody leaves.
	credential, err := store.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: employee.ID, Epoch: employee.AuthEpoch, Purpose: repo.PurposeCodexGateway,
		Ciphertext: []byte("sealed"), KeyVersion: "k1",
	})
	if err != nil {
		t.Fatalf("store credential: %v", err)
	}
	if _, err := store.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: employee.ID, Epoch: employee.AuthEpoch,
		ExternalUser: "emp-work1", KeyAlias: "work1-1", CredentialID: credential.ID,
	}); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	gone, err := svc.Offboard(ctx, employee.ID, "zhang", "req-2")
	if err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if gone.Active() || gone.AuthEpoch <= employee.AuthEpoch {
		t.Fatalf("offboarded employee = %+v", gone)
	}

	// What the database can settle by itself is settled immediately.
	if _, err := store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("the credential is still live after offboarding: %v", err)
	}
	if _, err := store.Grants().Active(ctx, "", employee.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("the grant is still wanted after offboarding: %v", err)
	}

	// What needs somebody else's agreement is a task, so a gateway that is
	// down delays the revocation rather than losing it.
	tasks := openTasks(t, ctx, store)
	if len(tasks) != 2 {
		t.Fatalf("open tasks = %v, want a revoke and an export", kinds(tasks))
	}
	wanted := map[string]bool{repo.TaskGatewayRevoke: false, repo.TaskOSSExport: false}
	for _, task := range tasks {
		if _, ok := wanted[task.Kind]; !ok {
			t.Errorf("unexpected task %s", task.Kind)
		}
		wanted[task.Kind] = true
		if task.TargetEpoch == nil || *task.TargetEpoch != gone.AuthEpoch {
			t.Errorf("%s is aimed at epoch %v, want the new %d", task.Kind, task.TargetEpoch, gone.AuthEpoch)
		}
	}
	for kind, seen := range wanted {
		if !seen {
			t.Errorf("no %s task was queued", kind)
		}
	}

	history, err := store.Audit().ByTarget(ctx, "employee", employee.ID, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(history) != 2 || history[0].Action != ActionOffboard {
		t.Errorf("audit = %v, want the offboarding on top", actions(history))
	}
}

func TestReissueReplacesTheOutstandingWork(t *testing.T) {
	svc, store, ctx := newService(t)
	employee, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "work1", Quota: testQuota(), Actor: "zhang",
	})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	// A worker takes the provisioning task and is still holding it.
	claimed, err := store.Tasks().Claim(ctx, "worker-1", []string{repo.TaskGatewayProvision}, 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	reissued, err := svc.Reissue(ctx, employee.ID, "zhang", "req-3")
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if reissued.AuthEpoch != employee.AuthEpoch+1 {
		t.Errorf("epoch = %d, want %d", reissued.AuthEpoch, employee.AuthEpoch+1)
	}

	// The in-flight task is superseded, and the worker's late result is
	// refused rather than applied to an epoch that has moved on.
	stale, err := store.Tasks().ByID(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if stale.Status != repo.TaskSuperseded {
		t.Errorf("the in-flight task is %s, want superseded", stale.Status)
	}
	if err := store.Tasks().Succeed(ctx, claimed.ID, "worker-1", "req"); !errors.Is(err, repo.ErrLeaseLost) {
		t.Errorf("a superseded task accepted its result: %v", err)
	}

	tasks := openTasks(t, ctx, store)
	if len(tasks) != 2 {
		t.Fatalf("open tasks = %v, want a fresh provision and export", kinds(tasks))
	}

	// Re-issuing a closed account is refused: it would leave a working token
	// for somebody who has left.
	if _, err := svc.Offboard(ctx, employee.ID, "zhang", ""); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if _, err := svc.Reissue(ctx, employee.ID, "zhang", ""); err == nil {
		t.Error("credentials were re-issued for a closed account")
	}
}

func TestQuotaAndModelChangesAreRecordedWithBeforeAndAfter(t *testing.T) {
	svc, store, ctx := newService(t)
	employee, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "work1", Quota: testQuota(), Models: []string{"claude-4.5-sonnet"}, Actor: "zhang",
	})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	current, err := store.Quotas().Get(ctx, employee.ID)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}

	raised := testQuota()
	raised.MonthlyBudget = "100"
	if err := svc.SetQuota(ctx, employee.ID, raised, current.Version, "li", "req-4"); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	// The second administrator's save, from a page rendered before the first,
	// is refused rather than silently applied over it.
	if err := svc.SetQuota(ctx, employee.ID, raised, current.Version, "wang", ""); !errors.Is(err, repo.ErrConflict) {
		t.Errorf("stale save: error = %v, want ErrConflict", err)
	}
	// A quota change does not re-issue anything: the gateway applies it to the
	// user, and making somebody sign in again for it would be gratuitous.
	after, err := store.Employees().ByID(ctx, employee.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.AuthEpoch != employee.AuthEpoch {
		t.Errorf("the epoch moved on a quota change: %d then %d", employee.AuthEpoch, after.AuthEpoch)
	}

	if err := svc.SetModels(ctx, employee.ID, []string{"gemini-2.5-pro"}, "li", ""); err != nil {
		t.Fatalf("set models: %v", err)
	}

	history, err := store.Audit().ByTarget(ctx, "employee", employee.ID, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if got := actions(history); len(got) != 3 || got[0] != ActionSetModels || got[1] != ActionSetQuota {
		t.Fatalf("audit = %v, want models, quota, onboard", got)
	}
	var before, afterQuota struct {
		MonthlyBudget string
		Models        []string
	}
	if err := json.Unmarshal(history[1].Before, &before); err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := json.Unmarshal(history[1].After, &afterQuota); err != nil {
		t.Fatalf("after: %v", err)
	}
	// The before-value is the only record of what a number used to be: the
	// quota table holds one row and nothing else remembers.
	if before.MonthlyBudget != "50.000000" || afterQuota.MonthlyBudget != "100.000000" {
		t.Errorf("audit recorded %q -> %q", before.MonthlyBudget, afterQuota.MonthlyBudget)
	}
}

func TestBindingAMachineMovesItInOnePiece(t *testing.T) {
	svc, store, ctx := newService(t)
	alice, err := svc.Onboard(ctx, OnboardSpec{WindowsUser: "work1", Quota: testQuota(), Actor: "zhang"})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	bob, err := svc.Onboard(ctx, OnboardSpec{WindowsUser: "work2", Quota: testQuota(), Actor: "zhang"})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}

	if _, err := svc.BindMachine(ctx, "DESKTOP-01", alice.ID, "第一台", "zhang", "req-5"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Handing the machine to somebody else is one transaction: a machine left
	// half reassigned is how one employee's credentials reach another.
	binding, err := svc.BindMachine(ctx, "desktop-01", bob.ID, "换人", "zhang", "")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if binding.EmployeeID != bob.ID || binding.Epoch != 2 {
		t.Fatalf("binding = %+v, want work2 at epoch 2", binding)
	}
	device, err := store.Devices().ByHostname(ctx, "desktop-01")
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	history, err := store.Bindings().History(ctx, device.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 || history[1].UnboundAt == nil {
		t.Errorf("binding history = %+v, want the first one closed", history)
	}

	// Both people need their delivered files rewritten -- the one who lost the
	// machine as much as the one who got it.
	tasks := openTasks(t, ctx, store)
	exports := map[string]bool{}
	for _, task := range tasks {
		if task.Kind == repo.TaskOSSExport {
			exports[task.EmployeeID] = true
		}
	}
	if !exports[alice.ID] || !exports[bob.ID] {
		t.Errorf("exports queued for %v, want both employees", exports)
	}

	nonce, err := svc.RequestCodexRestart(ctx, "desktop-01", "zhang", "")
	if err != nil {
		t.Fatalf("restart request: %v", err)
	}
	open, err := store.Bindings().Open(ctx, device.ID)
	if err != nil {
		t.Fatalf("open binding: %v", err)
	}
	if open.RestartNonce != nonce {
		t.Errorf("nonce = %q, want %q", open.RestartNonce, nonce)
	}

	if err := svc.UnbindMachine(ctx, "desktop-01", "zhang", ""); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if _, err := store.Bindings().Open(ctx, device.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("the machine is still bound: %v", err)
	}
	// A closed account must not be handed a machine.
	if _, err := svc.Offboard(ctx, bob.ID, "zhang", ""); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if _, err := svc.BindMachine(ctx, "desktop-01", bob.ID, "", "zhang", ""); err == nil {
		t.Error("a machine was assigned to a closed account")
	}
}

func TestPublishingAPolicyQueuesItsDelivery(t *testing.T) {
	svc, store, ctx := newService(t)

	published, err := svc.PublishPolicy(ctx,
		[]byte(`{"blockEnabled":true,"syncIntervalMinutes":30}`), "首个策略", "zhang", "req-6")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	current, err := store.Policies().Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current.Version != published.Version {
		t.Errorf("current = %d, want %d", current.Version, published.Version)
	}

	tasks := openTasks(t, ctx, store)
	if len(tasks) != 1 || tasks[0].Kind != repo.TaskOSSExport {
		t.Fatalf("queued %v, want one export", kinds(tasks))
	}
	// Publishing twice queues two exports, one per version: the agents read
	// one object, but the record of what was published is per version.
	if _, err := svc.PublishPolicy(ctx, []byte(`{"blockEnabled":false}`), "关掉拦截", "zhang", ""); err != nil {
		t.Fatalf("publish again: %v", err)
	}
	if tasks := openTasks(t, ctx, store); len(tasks) != 2 {
		t.Errorf("queued %v after a second publish", kinds(tasks))
	}

	history, err := store.Audit().Recent(ctx, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(history) != 2 || history[0].Action != ActionPublishPolicy {
		t.Errorf("audit = %v", actions(history))
	}
}

// A failure anywhere in an operation leaves none of it: that is the whole
// reason these are transactions.
func TestAFailedOperationLeavesNothingBehind(t *testing.T) {
	svc, store, ctx := newService(t)

	// A quota the gateway could never honour is refused by the repository,
	// after the employee row has already been written inside the transaction.
	_, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "work1",
		Quota:       repo.Quota{MonthlyBudget: "0", RPM: 60, TPM: 100, Parallel: 1},
		Actor:       "zhang",
	})
	if err == nil {
		t.Fatal("an account was opened on a zero budget")
	}

	employees, err := store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(employees) != 0 {
		t.Errorf("the failed onboarding left %d employees behind", len(employees))
	}
	if tasks := openTasks(t, ctx, store); len(tasks) != 0 {
		t.Errorf("the failed onboarding left tasks behind: %v", kinds(tasks))
	}
	events, err := store.Audit().Recent(ctx, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("the failed onboarding left %d audit events behind", len(events))
	}
}

func actions(events []repo.AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Action)
	}
	return out
}

// Keying an export on the employee and epoch alone looked tidy and was wrong:
// binding a machine does not change the epoch, so the key matched the export
// that had already run during onboarding, Enqueue handed back that finished
// task, and the new binding was never written.
func TestEveryThingThatMustReachADesktopQueuesItsOwnExport(t *testing.T) {
	svc, store, ctx := newService(t)
	employee, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "work1", Quota: testQuota(), Models: []string{"claude-4.5-sonnet"}, Actor: "zhang",
	})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	// Stand in for the Worker: everything queued so far has been done.
	finish(t, ctx, store)

	if _, err := svc.BindMachine(ctx, "desktop-01", employee.ID, "", "zhang", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got := len(openTasks(t, ctx, store)); got == 0 {
		t.Fatal("binding a machine queued no export at all")
	}
	finish(t, ctx, store)

	if _, err := svc.RequestCodexRestart(ctx, "desktop-01", "zhang", ""); err != nil {
		t.Fatalf("restart: %v", err)
	}
	first := openTasks(t, ctx, store)
	if len(first) != 1 {
		t.Fatalf("a restart request queued %v", kinds(first))
	}
	finish(t, ctx, store)

	// Asking again asks again: the point of a one-shot instruction is that a
	// second press reaches the machine a second time.
	if _, err := svc.RequestCodexRestart(ctx, "desktop-01", "zhang", ""); err != nil {
		t.Fatalf("second restart: %v", err)
	}
	second := openTasks(t, ctx, store)
	if len(second) != 1 {
		t.Fatalf("the second restart request queued %v", kinds(second))
	}
	if second[0].ID == first[0].ID {
		t.Error("the second restart request reused the first one's task")
	}

	finish(t, ctx, store)
	if err := svc.SetModels(ctx, employee.ID, []string{"gemini-2.5-pro"}, "zhang", ""); err != nil {
		t.Fatalf("set models: %v", err)
	}
	// The models decide what the picker shows as well as what the token may
	// call, so the delivered catalog has to be rewritten too.
	var sawExport bool
	for _, task := range openTasks(t, ctx, store) {
		if task.Kind == repo.TaskOSSExport {
			sawExport = true
		}
	}
	if !sawExport {
		t.Error("changing the models queued no export, so the picker would keep the old list")
	}
}

// finish marks everything queued as done, standing in for the Worker.
func finish(t *testing.T, ctx context.Context, store *dbstore.Store) {
	t.Helper()
	for range 20 {
		task, err := store.Tasks().Claim(ctx, "test-worker", nil, 0)
		if errors.Is(err, repo.ErrNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := store.Tasks().Succeed(ctx, task.ID, "test-worker", ""); err != nil {
			t.Fatalf("succeed: %v", err)
		}
	}
	t.Fatal("the queue did not empty")
}

// A quota change after onboarding has finished must reach the gateway. The
// provisioning task is keyed on the epoch, and a quota change does not move
// the epoch -- so the key matches the task that already ran.
func TestAQuotaChangeAfterOnboardingReachesTheGateway(t *testing.T) {
	svc, store, ctx := newService(t)
	employee, err := svc.Onboard(ctx, OnboardSpec{
		WindowsUser: "work1", Quota: testQuota(), Models: []string{"claude-4.5-sonnet"}, Actor: "zhang",
	})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	finish(t, ctx, store)

	current, err := store.Quotas().Get(ctx, employee.ID)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	raised := testQuota()
	raised.MonthlyBudget = "100"
	if err := svc.SetQuota(ctx, employee.ID, raised, current.Version, "zhang", ""); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	var provision bool
	for _, task := range openTasks(t, ctx, store) {
		if task.Kind == repo.TaskGatewayProvision {
			provision = true
		}
	}
	if !provision {
		t.Fatal("changing the quota queued no gateway work; the gateway would keep the old limits")
	}
}

func TestDeleteIsOnlyForClosedAccountsAndHidesThem(t *testing.T) {
	svc, store, ctx := newService(t)
	employee, err := svc.Onboard(ctx, OnboardSpec{WindowsUser: "work7", Quota: testQuota(), Actor: "zhang"})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if _, err := svc.Delete(ctx, employee.ID, "zhang", "r1"); err == nil {
		t.Fatal("an open account must not be deletable")
	}
	if _, err := svc.Offboard(ctx, employee.ID, "zhang", "r2"); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	before := len(openTasks(t, ctx, store))
	deleted, err := svc.Delete(ctx, employee.ID, "zhang", "r3")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleted.Deleted() {
		t.Fatalf("not marked deleted: %+v", deleted)
	}
	// The gateway user goes and the delivered files are withdrawn; both by
	// the Worker, both committed with the deletion.
	kinds := map[string]int{}
	for _, task := range openTasks(t, ctx, store) {
		if task.EmployeeID == employee.ID {
			kinds[task.Kind]++
		}
	}
	if kinds[repo.TaskGatewayDelete] != 1 || kinds[repo.TaskOSSExport] < 1 {
		t.Fatalf("queued after delete: %v (had %d open before)", kinds, before)
	}
	// Gone from the roster, and the name can be used again.
	if list, _ := store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true}); len(list) != 0 {
		t.Fatalf("roster still lists the deleted account: %+v", list)
	}
	fresh, err := svc.Onboard(ctx, OnboardSpec{WindowsUser: "work7", Quota: testQuota(), Actor: "zhang"})
	if err != nil || fresh.ID == employee.ID {
		t.Fatalf("re-onboarding the name: %+v, %v", fresh, err)
	}
	events, _ := store.Audit().ByTarget(ctx, "employee", employee.ID, 10)
	found := false
	for _, ev := range events {
		found = found || ev.Action == ActionDelete
	}
	if !found {
		t.Fatal("no audit line for the deletion")
	}
}
