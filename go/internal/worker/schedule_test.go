package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func (f *fakeObjects) List(prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for key := range f.objects {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			out = append(out, key)
		}
	}
	return out, nil
}

func TestThePeriodicJobsAreQueuedOncePerBucket(t *testing.T) {
	store, ctx := newWorkerStore(t)
	now := time.Date(2026, 9, 18, 10, 20, 0, 0, time.UTC)

	for range 3 {
		if err := enqueuePeriodic(ctx, store, TaskCatalogSync, now, EveryCatalogSync, 2); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if err := enqueuePeriodic(ctx, store, TaskCatalogSync, now.Add(time.Minute), EveryCatalogSync, 2); err != nil {
		t.Fatalf("enqueue next minute: %v", err)
	}
	tasks, err := store.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("%d tasks queued for two minutes, want 2", len(tasks))
	}
	for _, task := range tasks {
		if task.MaxAttempts != 2 {
			t.Errorf("a periodic job may be retried %d times; the next bucket supersedes it", task.MaxAttempts)
		}
	}
}

func TestScheduleStopsWithItsContext(t *testing.T) {
	store, ctx := newWorkerStore(t)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		Schedule(runCtx, store, func(err error) { t.Errorf("schedule: %v", err) })
		close(done)
	}()
	// The first tick is immediate.
	deadline := time.After(5 * time.Second)
	for {
		tasks, err := store.Tasks().ListOpen(ctx, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(tasks) >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d jobs queued by the first tick", len(tasks))
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Schedule did not stop with its context")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
