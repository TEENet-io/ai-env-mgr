package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/directory"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type fakeDirectory struct {
	list directory.List
	err  error
}

func (f *fakeDirectory) Fetch(context.Context) (directory.List, error) { return f.list, f.err }

func directoryAlerts(t *testing.T, ctx context.Context, store repo.Store) int {
	t.Helper()
	open, err := store.Alerts().ListOpen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range open {
		if a.Kind == repo.AlertDirectorySync {
			n++
		}
	}
	return n
}

// A tool that stops answering changes nothing here, and only after a
// quarter of an hour does somebody hear about it; the next good reading
// clears it.
func TestADirectoryThatStopsAnsweringIsAnAlertAfterAQuarterHour(t *testing.T) {
	store, database, ctx := newWorkerDB(t)
	dir := &fakeDirectory{list: directory.List{Accounts: []directory.Account{{
		ID: "00000000-0000-4000-8000-000000000001", Username: "owner", Name: "Owner", Email: "owner@example.com",
		Role: directory.RoleAdmin, Status: directory.StatusActive,
	}}}}
	clock := time.Now()
	h := DirectorySync{Store: store, Directory: dir, Copy: database.Directory(), Now: func() time.Time { return clock }}

	if res, err := h.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("first reading: %v", err)
	} else if res.Note == "" {
		t.Error("a reading says nothing about what it did")
	}

	dir.err = errors.New("HTTP 502")
	if _, err := h.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("a failed reading failed the task: %v", err)
	}
	if directoryAlerts(t, ctx, store) != 0 {
		t.Fatal("alerted on the first failure")
	}
	clock = clock.Add(DirectoryAlertAfter + time.Minute)
	if _, err := h.Run(ctx, repo.Task{}); err != nil {
		t.Fatal(err)
	}
	if directoryAlerts(t, ctx, store) != 1 {
		t.Fatal("no alert after a quarter of an hour of failures")
	}
	known, err := database.Directory().Known(ctx)
	if err != nil || len(known) != 1 || !known["00000000-0000-4000-8000-000000000001"].Active() {
		t.Errorf("failed readings changed the copy: %v %v", known, err)
	}

	dir.err = nil
	if _, err := h.Run(ctx, repo.Task{}); err != nil {
		t.Fatal(err)
	}
	if directoryAlerts(t, ctx, store) != 0 {
		t.Error("the alert stayed open after a good reading")
	}
}

// The job is queued only where a console asks for it.
func TestScheduleQueuesTheExtraJobsItIsGiven(t *testing.T) {
	store, ctx := newWorkerStore(t)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		Schedule(runCtx, store, func(err error) { t.Errorf("schedule: %v", err) },
			Periodic{Kind: TaskDirectorySync, Every: EveryDirectorySync, MaxAttempts: 1})
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for {
		tasks, err := store.Tasks().ListOpen(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, task := range tasks {
			found = found || task.Kind == TaskDirectorySync
		}
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the directory sync was not queued")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}
