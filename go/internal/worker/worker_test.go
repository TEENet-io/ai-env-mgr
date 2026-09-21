package worker

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func newWorkerStore(t *testing.T) (*dbstore.Store, context.Context) {
	t.Helper()
	store, _, ctx := newWorkerDB(t)
	return store, ctx
}

// newWorkerDB is newWorkerStore with the connection too, for a test that
// has to backdate a row the repositories rightly offer no way to backdate.
func newWorkerDB(t *testing.T) (*dbstore.Store, *dbstore.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN is not set; skipping the PostgreSQL integration tests")
	}
	ctx := context.Background()
	// A database of this package's own: `go test ./...` runs packages in
	// parallel and each of these suites empties the schema it is about to use.
	dsn, err := dbstore.TestDatabaseDSN(ctx, dsn, "aienv_test_worker")
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
	return dbstore.NewStore(database), database, ctx
}

func queue(t *testing.T, ctx context.Context, store *dbstore.Store, kind, key string) repo.Task {
	t.Helper()
	task, _, err := store.Tasks().Enqueue(ctx, repo.NewTask{Kind: kind, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("enqueue %s: %v", key, err)
	}
	return task
}

func collector() (func(Event), func() []Event) {
	var mu sync.Mutex
	var events []Event
	return func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}, func() []Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]Event(nil), events...)
		}
}

func TestAHandledTaskIsClosed(t *testing.T) {
	store, ctx := newWorkerStore(t)
	task := queue(t, ctx, store, repo.TaskGatewayProvision, "a")

	onEvent, events := collector()
	w := New(store, Options{Owner: "worker-1", OnEvent: onEvent})
	var seen repo.Task
	w.Register(repo.TaskGatewayProvision, HandlerFunc(func(_ context.Context, task repo.Task) (Result, error) {
		seen = task
		return Result{ExternalRef: "gateway-req-7"}, nil
	}))

	worked, err := w.RunOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("RunOnce = %v (%v), want a task to have been run", worked, err)
	}
	if seen.ID != task.ID {
		t.Fatalf("the handler was given %s, want %s", seen.ID, task.ID)
	}

	done, err := store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if done.Status != repo.TaskSucceeded {
		t.Errorf("task is %s, want succeeded", done.Status)
	}
	attempts, err := store.Tasks().Attempts(ctx, task.ID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	// The upstream's request id is what turns "the gateway rejected it" into
	// something the gateway's operator can look up.
	if len(attempts) != 1 || attempts[0].ExternalRef != "gateway-req-7" {
		t.Errorf("attempts = %+v", attempts)
	}
	if got := events(); len(got) != 1 || got[0].Outcome != "succeeded" {
		t.Errorf("events = %+v", got)
	}

	// An empty queue is not a fault.
	worked, err = w.RunOnce(ctx)
	if err != nil || worked {
		t.Errorf("RunOnce on an empty queue = %v (%v)", worked, err)
	}
}

func TestAFailingTaskComesBackWithBackoff(t *testing.T) {
	store, ctx := newWorkerStore(t)
	task := queue(t, ctx, store, repo.TaskGatewayProvision, "a")

	w := New(store, Options{Owner: "worker-1", BaseBackoff: time.Minute, MaxBackoff: time.Hour})
	w.Register(repo.TaskGatewayProvision, HandlerFunc(func(context.Context, repo.Task) (Result, error) {
		return Result{}, ClassError("upstream_5xx", errors.New("gateway returned 502"))
	}))

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	back, err := store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if back.Status != repo.TaskRetryWait {
		t.Fatalf("task is %s, want retry_wait", back.Status)
	}
	// Not immediately: an upstream that just failed is not helped by being
	// asked again in the same second.
	if !back.NextRunAt.After(time.Now().Add(20 * time.Second)) {
		t.Errorf("next run at %v, want it a while from now", back.NextRunAt)
	}
	if back.LastError == "" {
		t.Error("the failure did not record why")
	}
	attempts, err := store.Tasks().Attempts(ctx, task.ID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ErrorClass != "upstream_5xx" {
		t.Errorf("attempts = %+v, want the failure grouped by its class", attempts)
	}
	// It is not runnable yet, so the worker finds nothing rather than
	// hammering the same task.
	if worked, err := w.RunOnce(ctx); err != nil || worked {
		t.Errorf("RunOnce = %v (%v), want nothing to be due", worked, err)
	}
}

func TestSomeFailuresAreNotWorthRetrying(t *testing.T) {
	store, ctx := newWorkerStore(t)
	task := queue(t, ctx, store, repo.TaskGatewayProvision, "a")

	w := New(store, Options{Owner: "worker-1"})
	calls := 0
	w.Register(repo.TaskGatewayProvision, HandlerFunc(func(context.Context, repo.Task) (Result, error) {
		calls++
		return Result{}, Permanent(errors.New("the payload names an employee that does not exist"))
	}))

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	dead, err := store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	// Nine more attempts would be load without hope, and would bury the real
	// failures in a list that never empties.
	if dead.Status != repo.TaskFailed || dead.FinishedAt == nil {
		t.Fatalf("task = %+v, want it failed for good", dead)
	}
	if worked, _ := w.RunOnce(ctx); worked {
		t.Error("a permanently failed task was picked up again")
	}
	if calls != 1 {
		t.Errorf("the handler ran %d times", calls)
	}
}

func TestAKindNobodyHandlesFailsLoudly(t *testing.T) {
	store, ctx := newWorkerStore(t)
	task := queue(t, ctx, store, "something_nobody_registered", "a")

	onEvent, events := collector()
	w := New(store, Options{Owner: "worker-1", OnEvent: onEvent})

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// A deployment mistake, not a transient one. Letting the lease time out
	// would make it look like an upstream problem for the next hour.
	dead, err := store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if dead.Status != repo.TaskFailed {
		t.Errorf("task is %s, want failed", dead.Status)
	}
	attempts, err := store.Tasks().Attempts(ctx, task.ID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ErrorClass != "unregistered_kind" {
		t.Errorf("attempts = %+v", attempts)
	}
	if got := events(); len(got) != 1 || got[0].Outcome != "failed" {
		t.Errorf("events = %+v", got)
	}
}

func TestAPanickingHandlerDoesNotTakeTheQueueDown(t *testing.T) {
	store, ctx := newWorkerStore(t)
	bad := queue(t, ctx, store, repo.TaskGatewayProvision, "bad")
	good := queue(t, ctx, store, repo.TaskOSSExport, "good")

	w := New(store, Options{Owner: "worker-1"})
	w.Register(repo.TaskGatewayProvision, HandlerFunc(func(context.Context, repo.Task) (Result, error) {
		panic("a nil map somewhere")
	}))
	w.Register(repo.TaskOSSExport, HandlerFunc(func(context.Context, repo.Task) (Result, error) {
		return Result{}, nil
	}))

	for range 2 {
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	crashed, err := store.Tasks().ByID(ctx, bad.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if crashed.Status != repo.TaskFailed {
		t.Errorf("the panicking task is %s, want failed", crashed.Status)
	}
	done, err := store.Tasks().ByID(ctx, good.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if done.Status != repo.TaskSucceeded {
		t.Errorf("the task after it is %s; a bug in one handler took the queue down", done.Status)
	}
}

func TestALongTaskKeepsItsLease(t *testing.T) {
	store, ctx := newWorkerStore(t)
	task := queue(t, ctx, store, repo.TaskOSSExport, "a")

	// A lease shorter than the handler: without the heartbeat, a second worker
	// would take the task while the first is still uploading.
	w := New(store, Options{Owner: "worker-1", Lease: 3 * time.Second})
	w.Register(repo.TaskOSSExport, HandlerFunc(func(context.Context, repo.Task) (Result, error) {
		time.Sleep(4 * time.Second)
		return Result{}, nil
	}))

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	done, err := store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if done.Status != repo.TaskSucceeded {
		t.Fatalf("task is %s; the lease was not held for the length of the work", done.Status)
	}
}

func TestRunStopsWhenAskedAndSweepsAbandonedWork(t *testing.T) {
	store, ctx := newWorkerStore(t)
	task := queue(t, ctx, store, repo.TaskOSSExport, "a")

	// A worker that died holding it.
	if _, err := store.Tasks().Claim(ctx, "the-worker-that-died", nil, time.Millisecond); err != nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	w := New(store, Options{Owner: "worker-2", Poll: 10 * time.Millisecond, SweepEvery: time.Millisecond})
	ran := make(chan struct{}, 1)
	w.Register(repo.TaskOSSExport, HandlerFunc(func(context.Context, repo.Task) (Result, error) {
		select {
		case ran <- struct{}{}:
		default:
		}
		return Result{}, nil
	}))

	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan error, 1)
	go func() { stopped <- w.Run(runCtx) }()

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned task was never picked up")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}

	done, err := store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if done.Status != repo.TaskSucceeded {
		t.Errorf("task is %s, want succeeded", done.Status)
	}
}

func TestBackoffGrowsAndStaysJittered(t *testing.T) {
	w := New(nil, Options{BaseBackoff: time.Second, MaxBackoff: time.Minute})

	var previous time.Duration
	for attempt := 1; attempt <= 6; attempt++ {
		var min, max time.Duration
		for range 40 {
			d := w.backoff(attempt)
			if min == 0 || d < min {
				min = d
			}
			if d > max {
				max = d
			}
		}
		if max > time.Minute {
			t.Fatalf("attempt %d waited %v, past the cap", attempt, max)
		}
		// Jitter is not decoration: without it a hundred tasks that failed
		// together come back together, at the same upstream, at once.
		if min == max {
			t.Errorf("attempt %d always waits exactly %v", attempt, min)
		}
		if attempt > 1 && max <= previous {
			t.Errorf("attempt %d does not wait longer than attempt %d", attempt, attempt-1)
		}
		previous = max
	}
}
