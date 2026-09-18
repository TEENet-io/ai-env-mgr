package dbstore

import (
	"errors"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestRegisterMachineOnFirstSight(t *testing.T) {
	s, ctx := newTestStore(t)

	first, err := s.Devices().EnsureByHostname(ctx, "DESKTOP-01")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if first.Status != repo.DeviceLegacyOSS {
		t.Errorf("status = %q, want legacy_oss", first.Status)
	}
	// The spelling is kept: the agent's OSS keys are built from the name it
	// reports, and the console has to reproduce them exactly.
	if first.Hostname != "DESKTOP-01" {
		t.Errorf("hostname = %q, want it stored as reported", first.Hostname)
	}

	// Windows treats the name case-insensitively, so this is the same machine,
	// not a second one with its own binding and its own credentials.
	again, err := s.Devices().EnsureByHostname(ctx, "desktop-01")
	if err != nil {
		t.Fatalf("register again: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("a differently-cased host name registered a second machine: %s then %s", first.ID, again.ID)
	}
	found, err := s.Devices().ByHostname(ctx, "Desktop-01")
	if err != nil {
		t.Fatalf("ByHostname: %v", err)
	}
	if found.ID != first.ID {
		t.Errorf("ByHostname found %s, want %s", found.ID, first.ID)
	}

	if _, err := s.Devices().EnsureByHostname(ctx, "  "); err == nil {
		t.Error("a machine with a blank host name was registered")
	}
	if _, err := s.Devices().ByHostname(ctx, "never-seen"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("ByHostname for an unknown machine: error = %v, want ErrNotFound", err)
	}
}

func TestMarkSeenKeepsAKnownAgentVersion(t *testing.T) {
	s, ctx := newTestStore(t)
	d, err := s.Devices().EnsureByHostname(ctx, "desktop-01")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	at := time.Date(2026, 9, 18, 4, 0, 0, 0, time.UTC)
	if err := s.Devices().MarkSeen(ctx, d.ID, "1.2.15", at); err != nil {
		t.Fatalf("mark seen: %v", err)
	}
	seen, err := s.Devices().ByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if seen.AgentVersion != "1.2.15" {
		t.Errorf("agent version = %q, want 1.2.15", seen.AgentVersion)
	}
	if seen.LastSeenAt == nil || !seen.LastSeenAt.Equal(at) {
		t.Errorf("last seen = %v, want %v", seen.LastSeenAt, at)
	}

	// A report that does not mention the version is not a report that the
	// version is gone; a blank in the machine list reads as a broken agent.
	later := at.Add(time.Hour)
	if err := s.Devices().MarkSeen(ctx, d.ID, "", later); err != nil {
		t.Fatalf("mark seen without a version: %v", err)
	}
	seen, err = s.Devices().ByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if seen.AgentVersion != "1.2.15" {
		t.Errorf("agent version = %q; an empty report erased it", seen.AgentVersion)
	}
	if seen.LastSeenAt == nil || !seen.LastSeenAt.Equal(later) {
		t.Errorf("last seen = %v, want %v", seen.LastSeenAt, later)
	}

	err = s.Devices().MarkSeen(ctx, "6f1e9e6c-0000-4000-8000-000000000000", "1.2.15", later)
	if !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("MarkSeen for an unknown machine: error = %v, want ErrNotFound", err)
	}
}

func TestRevokedMachinesLeaveTheList(t *testing.T) {
	s, ctx := newTestStore(t)
	kept, _ := s.Devices().EnsureByHostname(ctx, "desktop-01")
	gone, _ := s.Devices().EnsureByHostname(ctx, "desktop-02")

	revoked, err := s.Devices().Revoke(ctx, gone.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked.Status != repo.DeviceRevoked || revoked.RevokedAt == nil {
		t.Fatalf("revoked = %+v, want a revoked status and a date", revoked)
	}

	// Revoking runs from a task, and a task runs at least once.
	twice, err := s.Devices().Revoke(ctx, gone.ID)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if !twice.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Error("the second revoke moved the revocation date")
	}

	list, err := s.Devices().List(ctx, repo.DeviceFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != kept.ID {
		t.Fatalf("list = %+v, want only desktop-01", list)
	}
	all, err := s.Devices().List(ctx, repo.DeviceFilter{IncludeRevoked: true})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("full list has %d machines, want 2", len(all))
	}
}

func TestOneOpenBindingPerMachine(t *testing.T) {
	s, ctx := newTestStore(t)
	alice := mustCreate(t, ctx, s, "work1")
	bob := mustCreate(t, ctx, s, "work2")
	d, _ := s.Devices().EnsureByHostname(ctx, "desktop-01")

	bound, err := s.Bindings().Bind(ctx, d.ID, alice.ID, "第一台", "admin")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.Epoch != 1 || !bound.Open() {
		t.Fatalf("binding = %+v, want an open binding at epoch 1", bound)
	}

	// Two open bindings would mean two credential sets racing into one
	// machine. Changing hands has to go through Unbind first.
	if _, err := s.Bindings().Bind(ctx, d.ID, bob.ID, "", "admin"); !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("binding an already-bound machine: error = %v, want ErrDuplicate", err)
	}

	err = s.InTx(ctx, func(tx repo.Store) error {
		if _, err := tx.Bindings().Unbind(ctx, d.ID, "admin"); err != nil {
			return err
		}
		_, err := tx.Bindings().Bind(ctx, d.ID, bob.ID, "换人", "admin")
		return err
	})
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}

	open, err := s.Bindings().Open(ctx, d.ID)
	if err != nil {
		t.Fatalf("open binding: %v", err)
	}
	if open.EmployeeID != bob.ID || open.Epoch != 2 {
		t.Errorf("open binding = %+v, want work2 at epoch 2", open)
	}

	// Closing rather than deleting is what keeps "who had this machine in
	// August" answerable.
	history, err := s.Bindings().History(ctx, d.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 || history[0].Epoch != 2 || history[1].Epoch != 1 {
		t.Fatalf("history = %+v, want both bindings newest first", history)
	}
	if history[1].UnboundAt == nil || history[1].UnboundBy != "admin" {
		t.Errorf("the closed binding does not record who closed it: %+v", history[1])
	}

	mine, err := s.Bindings().OpenByEmployee(ctx, bob.ID)
	if err != nil {
		t.Fatalf("OpenByEmployee: %v", err)
	}
	if len(mine) != 1 || mine[0].DeviceID != d.ID {
		t.Errorf("OpenByEmployee = %+v, want desktop-01", mine)
	}
	if left, err := s.Bindings().OpenByEmployee(ctx, alice.ID); err != nil || len(left) != 0 {
		t.Errorf("OpenByEmployee for the previous holder = %+v (%v), want none", left, err)
	}
}

func TestUnbindingAnUnboundMachine(t *testing.T) {
	s, ctx := newTestStore(t)
	d, _ := s.Devices().EnsureByHostname(ctx, "desktop-01")

	if _, err := s.Bindings().Open(ctx, d.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("Open on an unassigned machine: error = %v, want ErrNotFound", err)
	}
	if _, err := s.Bindings().Unbind(ctx, d.ID, "admin"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("Unbind on an unassigned machine: error = %v, want ErrNotFound", err)
	}
}

func TestCodexRestartRequestNeedsSomebodyToRestart(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	d, _ := s.Devices().EnsureByHostname(ctx, "desktop-01")

	// A machine nobody is assigned to has nobody whose Codex could be ended.
	// Succeeding at nothing would leave an administrator waiting for an effect
	// that is never coming.
	if _, err := s.Bindings().RequestCodexRestart(ctx, d.ID, "nonce-1"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("restart request on an unassigned machine: error = %v, want ErrNotFound", err)
	}

	if _, err := s.Bindings().Bind(ctx, d.ID, e.ID, "", "admin"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	asked, err := s.Bindings().RequestCodexRestart(ctx, d.ID, "nonce-1")
	if err != nil {
		t.Fatalf("restart request: %v", err)
	}
	if asked.RestartNonce != "nonce-1" || asked.RestartAt == nil {
		t.Fatalf("binding = %+v, want the nonce and the time recorded", asked)
	}
	if _, err := s.Bindings().RequestCodexRestart(ctx, d.ID, ""); err == nil {
		t.Error("an empty nonce was accepted; the agent would have nothing to compare")
	}

	// Reassigning the machine drops the request: it belongs to the person who
	// has just been unassigned, and ending the new employee's Codex would be
	// for a reason that has nothing to do with them.
	other := mustCreate(t, ctx, s, "work2")
	err = s.InTx(ctx, func(tx repo.Store) error {
		if _, err := tx.Bindings().Unbind(ctx, d.ID, "admin"); err != nil {
			return err
		}
		_, err := tx.Bindings().Bind(ctx, d.ID, other.ID, "", "admin")
		return err
	})
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	open, err := s.Bindings().Open(ctx, d.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if open.RestartNonce != "" {
		t.Errorf("the new binding carries the previous employee's restart request: %q", open.RestartNonce)
	}
}

func TestBindingRequiresRealRows(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	d, _ := s.Devices().EnsureByHostname(ctx, "desktop-01")
	missing := "6f1e9e6c-0000-4000-8000-000000000000"

	if _, err := s.Bindings().Bind(ctx, missing, e.ID, "", "admin"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("binding an unknown machine: error = %v, want ErrNotFound", err)
	}
	if _, err := s.Bindings().Bind(ctx, d.ID, missing, "", "admin"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("binding to an unknown employee: error = %v, want ErrNotFound", err)
	}
	if _, err := s.Bindings().Bind(ctx, "", e.ID, "", "admin"); err == nil {
		t.Error("binding with no machine was accepted")
	}
}
