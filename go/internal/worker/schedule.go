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

	EveryStatusImport = time.Minute
	EveryAuditPublish = time.Minute
	EveryReconcile    = time.Hour
)

// Schedule queues the periodic jobs until ctx is done. Run it alongside
// Worker.Run in the same process.
func Schedule(ctx context.Context, store repo.Store, onError func(error)) {
	tick := func() {
		now := time.Now().UTC()
		for _, job := range []struct {
			kind  string
			every time.Duration
			max   int
		}{
			{TaskStatusImport, EveryStatusImport, 2},
			{repo.TaskAuditPublish, EveryAuditPublish, 2},
			{repo.TaskReconcile, EveryReconcile, 3},
		} {
			if err := enqueuePeriodic(ctx, store, job.kind, now, job.every, job.max); err != nil && onError != nil {
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
	bucket := now.Truncate(every)
	_, _, err := store.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           kind,
		IdempotencyKey: kind + ":" + bucket.Format(time.RFC3339),
		NotBefore:      bucket,
		// A periodic job that keeps failing is superseded by the next bucket's;
		// retrying the old one for an hour would just double the load on
		// whatever is broken.
		MaxAttempts: maxAttempts,
	})
	return err
}
