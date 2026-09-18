package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func putStatus(t *testing.T, objects *fakeObjects, s model.Status) {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	objects.mu.Lock()
	defer objects.mu.Unlock()
	objects.objects[ossclient.StatusKey(s.Machine)] = data
}

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

func TestStatusImportRegistersMachinesAndKeepsTheNewest(t *testing.T) {
	store, ctx := newWorkerStore(t)
	objects := newFakeObjects()
	putStatus(t, objects, model.Status{
		Machine: "DESKTOP-01", BoundUser: "work1", LastSync: "2026-09-18T08:00:00Z",
		AgentVersion: "1.2.15", AppLockerMode: "audit", CodexRestartNonce: "n1",
		Errors: []string{"policy: boom"}, Warnings: []string{"a", "b"},
	})

	handler := StatusImport{Store: store, Objects: objects}
	result, err := handler.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Note == "" {
		t.Error("the import said nothing")
	}

	// Machines arrive by turning up.
	device, err := store.Devices().ByHostname(ctx, "desktop-01")
	if err != nil {
		t.Fatalf("the machine was not registered: %v", err)
	}
	if device.AgentVersion != "1.2.15" || device.LastSeenAt == nil {
		t.Errorf("device = %+v, want the version and last seen filled in", device)
	}
	report, err := store.Reports().Get(ctx, device.ID)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.ErrorCount != 1 || report.WarningCount != 2 || report.CodexRestartNonce != "n1" || report.AppLockerMode != "audit" {
		t.Errorf("report = %+v", report)
	}
	if report.LastSyncAt == nil || !report.LastSyncAt.Equal(time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("last sync = %v", report.LastSyncAt)
	}

	// An older report turning up later -- object stores do not promise read
	// order -- must not overwrite the newer one, or the machine looks like it
	// went dark.
	objects.mu.Lock()
	objects.objects[ossclient.StatusKey("DESKTOP-01")] = mustJSON(t, model.Status{
		Machine: "DESKTOP-01", LastSync: "2026-09-18T07:00:00Z", AgentVersion: "1.2.14",
	})
	objects.mu.Unlock()
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("second import: %v", err)
	}
	report, _ = store.Reports().Get(ctx, device.ID)
	if report.AgentVersion != "1.2.15" {
		t.Errorf("an older report overwrote the newer one: %+v", report)
	}

	// A broken object stops nothing else.
	objects.mu.Lock()
	objects.objects[ossclient.StatusKey("DESKTOP-02")] = []byte("{not json")
	objects.objects[ossclient.StatusKey("DESKTOP-03")] = mustJSON(t, model.Status{
		Machine: "DESKTOP-03", LastSync: "2026-09-18T09:00:00Z",
	})
	objects.mu.Unlock()
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("third import: %v", err)
	}
	if _, err := store.Devices().ByHostname(ctx, "DESKTOP-03"); err != nil {
		t.Errorf("a broken report for one machine stopped another from being imported: %v", err)
	}
}

func TestThePeriodicJobsAreQueuedOncePerBucket(t *testing.T) {
	store, ctx := newWorkerStore(t)
	now := time.Date(2026, 9, 18, 10, 20, 0, 0, time.UTC)

	for range 3 {
		if err := enqueuePeriodic(ctx, store, TaskStatusImport, now, EveryStatusImport, 2); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if err := enqueuePeriodic(ctx, store, TaskStatusImport, now.Add(time.Minute), EveryStatusImport, 2); err != nil {
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
