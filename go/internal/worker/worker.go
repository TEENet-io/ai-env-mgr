// Package worker carries out what ops decided.
//
// It claims tasks one at a time, runs the handler for the kind, and records
// what happened. Everything that talks to something outside the database --
// the gateway, OSS, the log archive -- happens here rather than in a request,
// because all of it can be slow, can fail halfway, and has to be retried
// without an administrator sitting there watching.
//
// Three rules the rest of the package is shaped around:
//
//   - A lease is not a lock. A worker that is killed holds nothing; its task
//     goes back on the queue when the lease runs out.
//
//   - An uncertain outcome is not a failure to repeat. When a call times out
//     we do not know whether it happened, so the answer is to ask the upstream
//     what it holds (reconciliation), not to do it again.
//
//   - Some failures are facts about the request. Retrying those nine more
//     times is load without hope, and it buries the real failures.
package worker

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Result is what a handler reports on success.
type Result struct {
	// ExternalRef is the upstream's own request id, when it gives one. It is
	// what turns "the gateway rejected it" into something the gateway's
	// operator can look up.
	ExternalRef string
	// Note goes into the task's last_error field on success -- which is to say,
	// it clears it. Kept for handlers that want to say what they did.
	Note string
}

// Handler runs one task.
//
// Returning an error means it did not work. Wrap it in Permanent when trying
// again cannot help; everything else is retried with backoff.
type Handler interface {
	Run(ctx context.Context, task repo.Task) (Result, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, task repo.Task) (Result, error)

func (f HandlerFunc) Run(ctx context.Context, task repo.Task) (Result, error) { return f(ctx, task) }

// permanentError marks a failure that will not go away by itself.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks an error as one that no number of retries can fix: a
// malformed payload, an upstream saying the request itself is wrong, a
// reference to something that has been deleted.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether an error was marked with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Options configure a Worker. Every field has a working default.
type Options struct {
	// Owner identifies this worker in leases and attempt rows. Default is the
	// host name and process id, which is what an incident needs.
	Owner string
	// Kinds narrows what this worker will take. Empty means anything.
	Kinds []string
	// Lease is how long a claim is held before another worker may take over.
	// It should comfortably exceed the slowest handler.
	Lease time.Duration
	// Poll is how long to wait after finding nothing to do.
	Poll time.Duration
	// BaseBackoff is the wait before the second attempt; it doubles from
	// there, with jitter, up to MaxBackoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// SweepEvery is how often expired leases are released and expired sessions
	// cleaned up. Zero disables the sweep, which only a test wants.
	SweepEvery time.Duration

	// OnEvent receives a line per finished task, for the unified log. It must
	// not block and must never be handed anything secret.
	OnEvent func(Event)

	now func() time.Time
}

// Event is one finished task, for logging.
type Event struct {
	Task        repo.Task
	Outcome     string // "succeeded", "retry", "failed", "superseded"
	Err         error
	ExternalRef string
	Duration    time.Duration
}

// Worker claims and runs tasks.
type Worker struct {
	store    repo.Store
	opts     Options
	handlers map[string]Handler
	mu       sync.RWMutex
}

// New builds a Worker with its defaults filled in.
func New(store repo.Store, opts Options) *Worker {
	if opts.Owner == "" {
		opts.Owner = defaultOwner()
	}
	if opts.Lease <= 0 {
		opts.Lease = 2 * time.Minute
	}
	if opts.Poll <= 0 {
		opts.Poll = 5 * time.Second
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = 10 * time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 10 * time.Minute
	}
	if opts.now == nil {
		opts.now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{store: store, opts: opts, handlers: map[string]Handler{}}
}

// Register attaches a handler to a task kind. Registering twice for one kind
// replaces the first, which is what a test wants and nothing else should do.
func (w *Worker) Register(kind string, h Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[kind] = h
}

// Owner is how this worker appears in leases and attempt rows.
func (w *Worker) Owner() string { return w.opts.Owner }

// Run works the queue until ctx is done.
//
// It runs in the console's own process: one small service, one database, and
// nothing gained by a second deployment to operate. If that changes, several
// Run loops against the same database are safe -- that is what the lease and
// SKIP LOCKED are for.
func (w *Worker) Run(ctx context.Context) error {
	var sweepAt time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		if w.opts.SweepEvery > 0 && w.opts.now().After(sweepAt) {
			w.sweep(ctx)
			sweepAt = w.opts.now().Add(w.opts.SweepEvery)
		}

		worked, err := w.RunOnce(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			// A database that cannot be reached is not a reason to spin: wait
			// the poll interval and try again, like an empty queue.
			if !sleep(ctx, w.opts.Poll) {
				return nil
			}
		case !worked:
			if !sleep(ctx, w.opts.Poll) {
				return nil
			}
		}
	}
}

// RunOnce claims and runs a single task. It reports whether there was one.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	task, err := w.store.Tasks().Claim(ctx, w.opts.Owner, w.opts.Kinds, w.opts.Lease)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return false, nil
		}
		return false, err
	}

	w.mu.RLock()
	handler, ok := w.handlers[task.Kind]
	w.mu.RUnlock()
	if !ok {
		// An unregistered kind is a deployment mistake, not a transient one.
		// Failing it says so; leaving it to time out would look like an
		// upstream problem for the next hour.
		w.finishPermanently(ctx, task, "unregistered_kind",
			fmt.Errorf("no handler is registered for %q", task.Kind), "")
		return true, nil
	}

	// The lease is extended while the handler runs, so a slow export is not
	// taken over half way through by another worker.
	stopHeartbeat := w.keepLeaseAlive(ctx, task.ID)
	started := w.opts.now()
	result, runErr := w.run(ctx, handler, task)
	stopHeartbeat()
	elapsed := w.opts.now().Sub(started)

	switch {
	case runErr == nil:
		w.finishSuccess(ctx, task, result, elapsed)
	case IsPermanent(runErr):
		w.finishPermanently(ctx, task, classify(runErr), runErr, result.ExternalRef)
	default:
		w.finishRetry(ctx, task, runErr, result.ExternalRef, elapsed)
	}
	return true, nil
}

// run calls the handler and turns a panic into an ordinary failure. A bug in
// one handler must not take the queue down with it.
func (w *Worker) run(ctx context.Context, handler Handler, task repo.Task) (result Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Permanent(fmt.Errorf("handler panicked: %v", r))
		}
	}()
	return handler.Run(ctx, task)
}

// recordingContext is used to write a task's outcome.
//
// It deliberately survives the cancellation of the worker's own context. The
// handler has already finished by this point, and the work it did is real: a
// shutdown that happens in between must not leave the task marked running and
// the result unrecorded, to be discovered and repeated an hour later.
func recordingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func (w *Worker) finishSuccess(ctx context.Context, task repo.Task, result Result, elapsed time.Duration) {
	ctx, cancel := recordingContext(ctx)
	defer cancel()
	if err := w.store.Tasks().Succeed(ctx, task.ID, w.opts.Owner, result.ExternalRef); err != nil {
		// The work was done but we could not say so. Reporting it loudly
		// matters: the task will be run again, and only an idempotency key
		// stands between that and doing it twice.
		w.emit(Event{Task: task, Outcome: "lost", Err: err, ExternalRef: result.ExternalRef, Duration: elapsed})
		return
	}
	w.emit(Event{Task: task, Outcome: "succeeded", ExternalRef: result.ExternalRef, Duration: elapsed})
}

func (w *Worker) finishRetry(ctx context.Context, task repo.Task, runErr error, externalRef string, elapsed time.Duration) {
	ctx, cancel := recordingContext(ctx)
	defer cancel()
	retryAt := w.opts.now().Add(w.backoff(task.Attempts))
	updated, err := w.store.Tasks().Fail(ctx, task.ID, w.opts.Owner, retryAt, classify(runErr), runErr.Error(), externalRef)
	if err != nil {
		w.emit(Event{Task: task, Outcome: "lost", Err: err, Duration: elapsed})
		return
	}
	outcome := "retry"
	if updated.Status == repo.TaskFailed {
		outcome = "failed"
	}
	w.emit(Event{Task: updated, Outcome: outcome, Err: runErr, ExternalRef: externalRef, Duration: elapsed})
}

func (w *Worker) finishPermanently(ctx context.Context, task repo.Task, class string, runErr error, externalRef string) {
	ctx, cancel := recordingContext(ctx)
	defer cancel()
	updated, err := w.store.Tasks().FailPermanently(ctx, task.ID, w.opts.Owner, class, runErr.Error(), externalRef)
	if err != nil {
		w.emit(Event{Task: task, Outcome: "lost", Err: err})
		return
	}
	w.emit(Event{Task: updated, Outcome: "failed", Err: runErr, ExternalRef: externalRef})
}

// keepLeaseAlive extends the claim while the handler runs and returns a
// function that stops doing so.
//
// Without it the lease would have to be set to the worst case of the slowest
// handler, and a worker that dies would then hold its task for that long.
func (w *Worker) keepLeaseAlive(ctx context.Context, taskID string) func() {
	interval := w.opts.Lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A lost lease is not worth shouting about here: whatever
				// took it will report its own outcome, and this handler's
				// result will be refused when it finishes.
				_ = w.store.Tasks().Extend(ctx, taskID, w.opts.Owner, w.opts.Lease)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// sweep does the housekeeping no single task owns: putting back tasks whose
// worker stopped, and clearing expired sessions.
func (w *Worker) sweep(ctx context.Context) {
	if n, err := w.store.Tasks().ReleaseExpiredLeases(ctx); err == nil && n > 0 {
		w.emit(Event{Outcome: "released", Err: fmt.Errorf("%d task(s) returned to the queue", n)})
	}
	_, _ = w.store.Admins().DeleteExpiredSessions(ctx)
}

// backoff is exponential with jitter, capped.
//
// The jitter is not decoration: without it, a hundred tasks that failed
// together come back together, and the upstream that was struggling gets the
// same burst again at exactly the same moment.
func (w *Worker) backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	wait := w.opts.BaseBackoff
	for range attempts - 1 {
		wait *= 2
		if wait >= w.opts.MaxBackoff {
			wait = w.opts.MaxBackoff
			break
		}
	}
	jitter := time.Duration(rand.Int64N(int64(wait/2) + 1))
	return wait/2 + jitter
}

func (w *Worker) emit(ev Event) {
	if w.opts.OnEvent != nil {
		w.opts.OnEvent(ev)
	}
}

// classify turns an error into a short, stable label for the attempt row.
// Handlers that know better attach their own by wrapping with ClassError.
func classify(err error) string {
	var c classedError
	if errors.As(err, &c) {
		return c.class
	}
	if IsPermanent(err) {
		return "permanent"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, repo.ErrNotFound) {
		return "not_found"
	}
	return "error"
}

type classedError struct {
	class string
	err   error
}

func (e classedError) Error() string { return e.err.Error() }
func (e classedError) Unwrap() error { return e.err }

// ClassError labels a failure so the attempt row groups with its kind:
// "upstream_5xx", "rate_limited", "oss_write". Grouping is what turns a list
// of failures into a diagnosis.
func ClassError(class string, err error) error {
	if err == nil {
		return nil
	}
	return classedError{class: class, err: err}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
