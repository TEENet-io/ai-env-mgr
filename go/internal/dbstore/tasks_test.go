package dbstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func enqueue(t *testing.T, ctx context.Context, s *Store, key string) repo.Task {
	t.Helper()
	task, created, err := s.Tasks().Enqueue(ctx, repo.NewTask{
		Kind: repo.TaskGatewayProvision, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", key, err)
	}
	if !created {
		t.Fatalf("enqueue %s: the task already existed", key)
	}
	return task
}

func TestEnqueueIsIdempotent(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	epoch := e.AuthEpoch

	first, created, err := s.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           repo.TaskGatewayProvision,
		IdempotencyKey: "work1:1:provision",
		Payload:        []byte(`{"employee":"work1"}`),
		TargetEpoch:    &epoch,
		EmployeeID:     e.ID,
	})
	if err != nil || !created {
		t.Fatalf("enqueue: %v, created=%v", err, created)
	}
	if first.Status != repo.TaskPending || first.Attempts != 0 || first.MaxAttempts != defaultMaxAttempts {
		t.Errorf("new task = %+v, want a pending task with no attempts", first)
	}

	// A retry after an ambiguous failure, or a double-clicked button, must
	// result in one piece of work -- not a second token for the same person.
	again, created, err := s.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           repo.TaskGatewayProvision,
		IdempotencyKey: "work1:1:provision",
		Payload:        []byte(`{"employee":"work1","note":"a second attempt"}`),
		TargetEpoch:    &epoch,
		EmployeeID:     e.ID,
	})
	if err != nil {
		t.Fatalf("enqueue again: %v", err)
	}
	if created {
		t.Error("the second enqueue created a task")
	}
	if again.ID != first.ID {
		t.Errorf("second enqueue returned %s, want the existing %s", again.ID, first.ID)
	}
	open, err := s.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 {
		t.Errorf("there are %d open tasks, want 1", len(open))
	}

	if _, _, err := s.Tasks().Enqueue(ctx, repo.NewTask{Kind: "", IdempotencyKey: "x"}); err == nil {
		t.Error("a task with no kind was accepted")
	}
	if _, _, err := s.Tasks().Enqueue(ctx, repo.NewTask{Kind: "x"}); err == nil {
		t.Error("a task with no idempotency key was accepted")
	}
}

func TestClaimGivesOneTaskToOneWorker(t *testing.T) {
	s, ctx := newTestStore(t)
	enqueue(t, ctx, s, "a")
	enqueue(t, ctx, s, "b")

	// Two workers polling at the same moment must not both get the same task:
	// that is two gateway calls for one change.
	var wg sync.WaitGroup
	claimed := make([]repo.Task, 2)
	errs := make([]error, 2)
	owners := []string{"worker-1", "worker-2"}
	for i := range owners {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed[i], errs[i] = s.Tasks().Claim(ctx, owners[i], nil, time.Minute)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("%s claim: %v", owners[i], err)
		}
	}
	if claimed[0].ID == claimed[1].ID {
		t.Fatalf("both workers claimed task %s", claimed[0].ID)
	}
	for i, task := range claimed {
		if task.Status != repo.TaskRunning || task.LeaseOwner != owners[i] || task.Attempts != 1 {
			t.Errorf("%s claimed %+v, want a running task leased to it at attempt 1", owners[i], task)
		}
		if task.LeaseUntil == nil || !task.LeaseUntil.After(time.Now()) {
			t.Errorf("%s claimed a task with no live lease: %v", owners[i], task.LeaseUntil)
		}
	}

	// Nothing left to do is the ordinary case, not a fault.
	if _, err := s.Tasks().Claim(ctx, "worker-3", nil, time.Minute); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("claiming from an empty queue: error = %v, want ErrNotFound", err)
	}

	// The attempt is recorded by the same statement that claims, so an
	// incident can see who was working on what.
	attempts, err := s.Tasks().Attempts(ctx, claimed[0].ID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Owner != owners[0] || attempts[0].Attempt != 1 {
		t.Errorf("attempts = %+v, want one attempt by %s", attempts, owners[0])
	}
}

func TestClaimHonoursKindAndSchedule(t *testing.T) {
	s, ctx := newTestStore(t)
	if _, _, err := s.Tasks().Enqueue(ctx, repo.NewTask{
		Kind: repo.TaskOSSExport, IdempotencyKey: "export",
	}); err != nil {
		t.Fatalf("enqueue export: %v", err)
	}
	if _, _, err := s.Tasks().Enqueue(ctx, repo.NewTask{
		Kind: repo.TaskGatewayProvision, IdempotencyKey: "later",
		NotBefore: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("enqueue delayed: %v", err)
	}

	if _, err := s.Tasks().Claim(ctx, "w", []string{repo.TaskGatewayRevoke}, time.Minute); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("claiming a kind nobody queued: error = %v, want ErrNotFound", err)
	}
	// A task scheduled for later is not runnable now, whatever its kind.
	if _, err := s.Tasks().Claim(ctx, "w", []string{repo.TaskGatewayProvision}, time.Minute); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("claiming a task due in an hour: error = %v, want ErrNotFound", err)
	}
	got, err := s.Tasks().Claim(ctx, "w", []string{repo.TaskOSSExport}, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.Kind != repo.TaskOSSExport {
		t.Errorf("claimed a %s, want an %s", got.Kind, repo.TaskOSSExport)
	}
}

func TestWorkOvertakenByANewEpochIsNotRun(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	epoch := e.AuthEpoch
	if _, _, err := s.Tasks().Enqueue(ctx, repo.NewTask{
		Kind: repo.TaskGatewayProvision, IdempotencyKey: "work1:1:provision",
		TargetEpoch: &epoch, EmployeeID: e.ID,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The employee leaves before the Worker gets to it. Running the old task
	// now would hand a working token back to somebody who has gone.
	if _, err := s.Employees().Offboard(ctx, e.ID, e.Version); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if _, err := s.Tasks().Claim(ctx, "w", nil, time.Minute); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("a task for a passed epoch was claimed: error = %v, want ErrNotFound", err)
	}

	// Offboarding closes the old work explicitly as well, in its own
	// transaction, so the list does not fill with tasks that will never run.
	after, err := s.Employees().ByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	n, err := s.Tasks().SupersedeOpenForEmployee(ctx, e.ID, after.AuthEpoch)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if n != 1 {
		t.Errorf("superseded %d tasks, want 1", n)
	}
	open, err := s.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("%d tasks are still open after the epoch moved on", len(open))
	}
}

func TestSucceedAndFailNeedALiveLease(t *testing.T) {
	s, ctx := newTestStore(t)
	enqueue(t, ctx, s, "a")

	task, err := s.Tasks().Claim(ctx, "worker-1", nil, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Another worker's result is not this task's result.
	if err := s.Tasks().Succeed(ctx, task.ID, "worker-2", "req-1"); !errors.Is(err, repo.ErrLeaseLost) {
		t.Errorf("completing somebody else's task: error = %v, want ErrLeaseLost", err)
	}
	if err := s.Tasks().Succeed(ctx, task.ID, "worker-1", "req-1"); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	done, err := s.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if done.Status != repo.TaskSucceeded || done.FinishedAt == nil || done.LeaseOwner != "" {
		t.Errorf("finished task = %+v, want succeeded with the lease released", done)
	}
	// Reporting the same result twice is not an extra success.
	if err := s.Tasks().Succeed(ctx, task.ID, "worker-1", "req-1"); !errors.Is(err, repo.ErrLeaseLost) {
		t.Errorf("completing a closed task: error = %v, want ErrLeaseLost", err)
	}

	attempts, err := s.Tasks().Attempts(ctx, task.ID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != "succeeded" || attempts[0].ExternalRef != "req-1" {
		t.Errorf("attempts = %+v, want one success carrying the upstream request id", attempts)
	}
}

func TestFailRetriesUntilTheAttemptsRunOut(t *testing.T) {
	s, ctx := newTestStore(t)
	created, _, err := s.Tasks().Enqueue(ctx, repo.NewTask{
		Kind: repo.TaskGatewayProvision, IdempotencyKey: "a", MaxAttempts: 2,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, err := s.Tasks().Claim(ctx, "w", nil, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	retryAt := time.Now().Add(-time.Second) // due immediately, so the test can go on
	back, err := s.Tasks().Fail(ctx, claimed.ID, "w", retryAt, "upstream_5xx", "gateway returned 502", "req-1")
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if back.Status != repo.TaskRetryWait || back.LeaseOwner != "" || back.FinishedAt != nil {
		t.Fatalf("after one failure = %+v, want it back on the queue", back)
	}
	if back.LastError == "" {
		t.Error("the failure did not record why")
	}

	claimed, err = s.Tasks().Claim(ctx, "w", nil, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed.Attempts != 2 {
		t.Errorf("attempts = %d on the second claim, want 2", claimed.Attempts)
	}
	// Retrying for ever turns one broken task into a permanent load on
	// whatever it is calling, and hides the fault in a list that never empties.
	dead, err := s.Tasks().Fail(ctx, claimed.ID, "w", retryAt, "upstream_5xx", "gateway returned 502", "req-2")
	if err != nil {
		t.Fatalf("second fail: %v", err)
	}
	if dead.Status != repo.TaskFailed || dead.FinishedAt == nil {
		t.Fatalf("after the last attempt = %+v, want a failed task", dead)
	}
	if _, err := s.Tasks().Claim(ctx, "w", nil, time.Minute); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("a failed task was claimed again: %v", err)
	}

	attempts, err := s.Tasks().Attempts(ctx, created.ID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want both of them kept", len(attempts))
	}
	if attempts[0].Outcome != "retry" || attempts[1].Outcome != "failed" {
		t.Errorf("outcomes = %q, %q; want retry then failed", attempts[0].Outcome, attempts[1].Outcome)
	}
	if attempts[0].ErrorClass != "upstream_5xx" || attempts[1].ExternalRef != "req-2" {
		t.Errorf("attempts lost their detail: %+v", attempts)
	}
}

func TestADeadWorkerReleasesItsTasks(t *testing.T) {
	s, ctx := newTestStore(t)
	enqueue(t, ctx, s, "a")

	// A lease, not a lock: a worker that is killed holds nothing. Without
	// this the task would sit in 'running' for ever with nobody on it.
	claimed, err := s.Tasks().Claim(ctx, "doomed-worker", nil, time.Millisecond)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	released, err := s.Tasks().ReleaseExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released != 1 {
		t.Fatalf("released %d tasks, want 1", released)
	}
	back, err := s.Tasks().ByID(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if back.Status != repo.TaskRetryWait || back.LeaseOwner != "" {
		t.Errorf("released task = %+v, want it back on the queue with no owner", back)
	}
	// The dead worker's report must not be accepted afterwards: the task may
	// already have been re-run by somebody else.
	if err := s.Tasks().Succeed(ctx, claimed.ID, "doomed-worker", ""); !errors.Is(err, repo.ErrLeaseLost) {
		t.Errorf("a dead worker's result was accepted: %v", err)
	}
	attempts, err := s.Tasks().Attempts(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ErrorClass != "lease_expired" {
		t.Errorf("attempts = %+v, want the abandoned attempt marked", attempts)
	}

	next, err := s.Tasks().Claim(ctx, "another-worker", nil, time.Minute)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if next.ID != claimed.ID || next.Attempts != 2 {
		t.Errorf("re-claimed %+v, want the same task on its second attempt", next)
	}
}

func TestExtendKeepsALongTaskFromBeingTakenTwice(t *testing.T) {
	s, ctx := newTestStore(t)
	enqueue(t, ctx, s, "a")
	claimed, err := s.Tasks().Claim(ctx, "w", nil, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.Tasks().Extend(ctx, claimed.ID, "w", time.Minute); err != nil {
		t.Fatalf("extend: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	released, err := s.Tasks().ReleaseExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released != 0 {
		t.Errorf("released %d tasks; the extended lease was ignored", released)
	}
	if err := s.Tasks().Extend(ctx, claimed.ID, "somebody-else", time.Minute); !errors.Is(err, repo.ErrLeaseLost) {
		t.Errorf("extending somebody else's lease: error = %v, want ErrLeaseLost", err)
	}
}

func TestSupersedeStopsWorkThatNoLongerMakesSense(t *testing.T) {
	s, ctx := newTestStore(t)
	task := enqueue(t, ctx, s, "a")

	if err := s.Tasks().Supersede(ctx, task.ID, "the policy was rolled back"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	after, err := s.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.Status != repo.TaskSuperseded || after.Open() {
		t.Errorf("task = %+v, want it superseded and closed", after)
	}
	// Superseding a task that is already closed changes nothing, and saying so
	// is better than reporting success for an effect that did not happen.
	if err := s.Tasks().Supersede(ctx, task.ID, "again"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("superseding a closed task: error = %v, want ErrNotFound", err)
	}
}

func TestAskingAgainRevivesATaskThatFailedForGood(t *testing.T) {
	s, ctx := newTestStore(t)
	first, created, err := s.Tasks().Enqueue(ctx, repo.NewTask{Kind: "gateway_revoke", IdempotencyKey: "gateway_revoke:e:1"})
	if err != nil || !created {
		t.Fatalf("enqueue: %+v %v %v", first, created, err)
	}
	claimed, err := s.Tasks().Claim(ctx, "w1", nil, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Tasks().FailPermanently(ctx, claimed.ID, "w1", "gateway_400", "no such key", ""); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got, _ := s.Tasks().ByID(ctx, first.ID); got.Status != repo.TaskFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	// The administrator presses the button again.
	again, created, err := s.Tasks().Enqueue(ctx, repo.NewTask{Kind: "gateway_revoke", IdempotencyKey: "gateway_revoke:e:1"})
	if err != nil || !created || again.ID != first.ID || again.Status != repo.TaskPending || again.LastError != "" {
		t.Fatalf("re-enqueue after failure: %+v created=%v err=%v", again, created, err)
	}
	if _, err := s.Tasks().Claim(ctx, "w2", nil, time.Minute); err != nil {
		t.Fatalf("the revived task must be claimable: %v", err)
	}
	// A task that succeeded is not run again.
	done, _, _ := s.Tasks().Enqueue(ctx, repo.NewTask{Kind: "x", IdempotencyKey: "x:1"})
	c, _ := s.Tasks().Claim(ctx, "w3", []string{"x"}, time.Minute)
	s.Tasks().Succeed(ctx, c.ID, "w3", "")
	same, created, _ := s.Tasks().Enqueue(ctx, repo.NewTask{Kind: "x", IdempotencyKey: "x:1"})
	if created || same.ID != done.ID || same.Status != repo.TaskSucceeded {
		t.Fatalf("a succeeded task must be returned as it is: %+v created=%v", same, created)
	}
}
