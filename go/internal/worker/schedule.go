package worker

import (
	"context"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Periodic kinds of work and how often they are queued.
//
// The queue does the running; this only decides when a job is due. Each job
// is keyed on the time bucket it belongs to, so a scheduler that fires twice
// -- or two consoles that both decide it is time -- produce one task, and a
// console that was down for an hour does not catch up by running sixty of
// them.
const (
	// TaskStatusImport copies machine reports in from OSS.
	TaskStatusImport = "status_import"
	// TaskReleaseScan registers packages CI put in the bucket.
	TaskReleaseScan = "release_scan"
	// TaskUsageSnapshot copies the gateway's usage into usage_daily.
	TaskUsageSnapshot = "usage_snapshot"
	// TaskAlertEval runs the alert rules.
	TaskAlertEval = "alert_eval"
	// TaskAlertNotify delivers open alerts.
	TaskAlertNotify = "alert_notify"

	EveryStatusImport  = time.Minute
	EveryReleaseScan   = 24 * time.Hour
	EveryUsageSnapshot = 24 * time.Hour
	EveryAlertEval     = 10 * time.Minute
	EveryAlertNotify   = time.Minute
	// The snapshot waits for the log service to finish indexing the day
	// that just ended; half an hour is generous.
	AfterUsageSnapshot = 30 * time.Minute
	EveryAuditPublish  = time.Minute
	EveryReconcile     = time.Hour
)

// Schedule queues the periodic jobs until ctx is done. Run it alongside
// Worker.Run in the same process.
func Schedule(ctx context.Context, store repo.Store, onError func(error)) {
	tick := func() {
		now := time.Now().UTC()
		for _, job := range []struct {
			kind  string
			every time.Duration
			after time.Duration
			max   int
		}{
			{TaskStatusImport, EveryStatusImport, 0, 2},
			{TaskReleaseScan, EveryReleaseScan, 0, 2},
			{TaskUsageSnapshot, EveryUsageSnapshot, AfterUsageSnapshot, 2},
			{TaskAlertEval, EveryAlertEval, 0, 1},
			{TaskAlertNotify, EveryAlertNotify, 0, 1},
			{repo.TaskAuditPublish, EveryAuditPublish, 0, 2},
			{repo.TaskReconcile, EveryReconcile, 0, 3},
		} {
			if ctx.Err() != nil {
				// Stopping is not an error, and an enqueue cut off by the
				// stop is not one either.
				return
			}
			if err := enqueuePeriodicAfter(ctx, store, job.kind, now, job.every, job.after, job.max); err != nil && onError != nil && ctx.Err() == nil {
				onError(err)
			}
		}
	}
	tick()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// enqueuePeriodic adds the job for the bucket that now falls in. The key is
// the bucket, which is what makes the second call in one bucket a no-op.
func enqueuePeriodic(ctx context.Context, store repo.Store, kind string, now time.Time, every time.Duration, maxAttempts int) error {
	return enqueuePeriodicAfter(ctx, store, kind, now, every, 0, maxAttempts)
}

// enqueuePeriodicAfter is enqueuePeriodic with the first attempt held until
// after minutes into the bucket, for a job that wants the previous bucket's
// data settled first.
func enqueuePeriodicAfter(ctx context.Context, store repo.Store, kind string, now time.Time, every, after time.Duration, maxAttempts int) error {
	bucket := now.Truncate(every)
	_, _, err := store.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           kind,
		IdempotencyKey: kind + ":" + bucket.Format(time.RFC3339),
		NotBefore:      bucket.Add(after),
		// A periodic job that keeps failing is superseded by the next bucket's;
		// retrying the old one for an hour would just double the load on
		// whatever is broken.
		MaxAttempts: maxAttempts,
	})
	return err
}
