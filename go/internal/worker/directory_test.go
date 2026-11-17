package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/directory"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
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

// With the switch on, somebody disabled in the tool loses their AI workspace
// account at the next reading, recorded as a close the system made, and a
// person is told to deal with the instance; off, only told to close it.
func TestADisabledPersonsAccountIsClosedOnlyWhenAllowed(t *testing.T) {
	for _, auto := range []bool{false, true} {
		store, database, ctx := newWorkerDB(t)
		amy := directory.Account{ID: "00000000-0000-4000-8000-000000000002", Username: "amy", Name: "Amy",
			Email: "amy@example.com", Role: directory.RoleEmployee, Status: directory.StatusActive}
		boss := directory.Account{ID: "00000000-0000-4000-8000-000000000001", Username: "boss", Name: "Boss",
			Email: "boss@example.com", Role: directory.RoleAdmin, Status: directory.StatusActive}
		dir := &fakeDirectory{list: directory.List{Accounts: []directory.Account{boss, amy}}}
		svc := ops.New(store)
		h := DirectorySync{Store: store, Directory: dir, Copy: database.Directory()}
		if auto {
			h.Offboard = svc.OffboardForDirectory
		}
		if _, err := h.Run(ctx, repo.Task{}); err != nil {
			t.Fatal(err)
		}
		emp, err := svc.Onboard(ctx, ops.OnboardSpec{WindowsUser: "work2", Name: "Amy", Email: amy.Email, DirectoryID: amy.ID, Actor: "admin",
			Quota: repo.Quota{MonthlyBudget: "20", RPM: 1, TPM: 1, Parallel: 1}})
		if err != nil {
			t.Fatal(err)
		}

		amy.Status = directory.StatusDisabled
		dir.list.Accounts = []directory.Account{boss, amy}
		if _, err := h.Run(ctx, repo.Task{}); err != nil {
			t.Fatalf("auto=%v: %v", auto, err)
		}
		after, err := store.Employees().ByID(ctx, emp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Active() == auto {
			t.Errorf("auto=%v: account active = %v", auto, after.Active())
		}
		var titles []string
		open, _ := store.Alerts().ListOpen(ctx)
		for _, a := range open {
			if a.Kind == repo.AlertDirectoryFollowUp {
				titles = append(titles, a.Title)
			}
		}
		want := "定製工具帳號變更需要人工處理：Amy"
		if auto {
			want = "系統聯動關戶：Amy"
		}
		if len(titles) != 1 || titles[0] != want {
			t.Errorf("auto=%v: follow-ups = %q", auto, titles)
		}
		if !auto {
			continue
		}
		history, err := store.Audit().ByTarget(ctx, "employee", emp.ID, 0)
		if err != nil || len(history) == 0 || history[0].Action != ops.ActionSyncOffboard || history[0].ActorID != "directory-sync" {
			t.Errorf("audit = %+v %v", history, err)
		}
		// A second reading finds nothing left to close.
		if res, err := h.Run(ctx, repo.Task{}); err != nil || strings.Contains(res.Note, "closed") {
			t.Errorf("again: %q %v", res.Note, err)
		}
	}
}

// One account that cannot be closed does not keep the others open, and does
// not fail the reading: it is an alert, and is tried again next time.
func TestOneCloseThatFailsDoesNotStopTheOthers(t *testing.T) {
	store, database, ctx := newWorkerDB(t)
	svc := ops.New(store)
	people := make([]directory.Account, 3)
	for i := range people {
		people[i] = directory.Account{ID: fmt.Sprintf("00000000-0000-4000-8000-00000000001%d", i), Username: fmt.Sprintf("p%d", i),
			Name: fmt.Sprintf("P%d", i), Role: directory.RoleEmployee, Status: directory.StatusActive}
	}
	dir := &fakeDirectory{list: directory.List{Accounts: append([]directory.Account(nil), people...)}}
	h := DirectorySync{Store: store, Directory: dir, Copy: database.Directory()}
	if _, err := h.Run(ctx, repo.Task{}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for i, p := range people {
		e, err := svc.Onboard(ctx, ops.OnboardSpec{WindowsUser: fmt.Sprintf("work%d", i), DirectoryID: p.ID, Actor: "admin",
			Quota: repo.Quota{MonthlyBudget: "20", RPM: 1, TPM: 1, Parallel: 1}})
		if err != nil {
			t.Fatal(err)
		}
		ids[e.ID] = p.Username
		people[i].Status = directory.StatusDisabled
	}
	dir.list.Accounts = people
	h.Offboard = func(ctx context.Context, id string) (repo.Employee, error) {
		if ids[id] == "p0" {
			return repo.Employee{}, errors.New("version conflict")
		}
		return svc.OffboardForDirectory(ctx, id)
	}
	res, err := h.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("the reading failed: %v", err)
	}
	if !strings.Contains(res.Note, "2 closed") || !strings.Contains(res.Note, "could not close work0") {
		t.Errorf("note = %q", res.Note)
	}
	open, _ := store.Alerts().ListOpen(ctx)
	failedAlerts := 0
	for _, a := range open {
		if strings.HasPrefix(a.Title, "系統聯動關戶失敗") {
			failedAlerts++
		}
	}
	if failedAlerts != 1 {
		t.Errorf("%d failure alerts", failedAlerts)
	}
}
