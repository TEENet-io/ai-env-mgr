package admincore

import (
	"testing"
	"time"

	"github.com/TEENet-io/airlock/internal/model"
	"github.com/TEENet-io/airlock/internal/ossclient"
	"github.com/TEENet-io/airlock/internal/status"
)

func statusBytes(t *testing.T, s model.Status) []byte {
	t.Helper()
	data, err := status.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestCollectMachinesHealthy covers the normal case: a machine that is
// bound, has reported recently, and whose report matches the binding.
func TestCollectMachinesHealthy(t *testing.T) {
	m, store := newManager()
	if err := m.AddUser("alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	store.objects[ossclient.StatusKey("work1")] = statusBytes(t, model.Status{
		Machine:    "work1",
		BoundUser:  "alice",
		LocalUsers: []string{"alice", "Administrator"},
		LastSync:   time.Now().UTC().Format(time.RFC3339),
	})

	states, err := m.CollectMachines(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected 1 machine, got %d", len(states))
	}
	s := states[0]
	if s.Machine != "work1" {
		t.Errorf("Machine = %q, want work1", s.Machine)
	}
	if !s.Bound {
		t.Error("expected Bound = true")
	}
	if s.Stale {
		t.Error("recently reported machine should not be Stale")
	}
	if s.Missing {
		t.Error("machine that reported should not be Missing")
	}
	if s.Unbound {
		t.Error("bound machine should not be Unbound")
	}
	if s.UserMissing {
		t.Error("bound employee is in LocalUsers, should not be UserMissing")
	}
	if len(s.ExtraUsers) != 0 {
		t.Errorf("ExtraUsers = %v, want none", s.ExtraUsers)
	}
}

func TestCollectMachinesStale(t *testing.T) {
	m, store := newManager()
	if err := m.AddUser("alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	store.objects[ossclient.StatusKey("work1")] = statusBytes(t, model.Status{
		Machine:    "work1",
		BoundUser:  "alice",
		LocalUsers: []string{"alice"},
		LastSync:   time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})

	states, err := m.CollectMachines(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !states[0].Stale {
		t.Error("report is 2h old against a 1h threshold and should be Stale")
	}
	if states[0].Missing {
		t.Error("a stale-but-present report should not be Missing")
	}
}

// A binding with no matching status object means the machine has never
// reported: either it is not on yet, or the agent was never installed.
func TestCollectMachinesMissing(t *testing.T) {
	m, _ := newManager()
	if err := m.AddUser("alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}

	states, err := m.CollectMachines(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected 1 machine, got %d", len(states))
	}
	if !states[0].Missing {
		t.Error("bound machine with no status report should be Missing")
	}
	if !states[0].Stale {
		t.Error("a machine with no report at all should also count as Stale")
	}
}

// A status object with no matching binding means the machine reported in
// but nobody has told it which employee it serves yet.
func TestCollectMachinesUnbound(t *testing.T) {
	m, store := newManager()
	store.objects[ossclient.StatusKey("work1")] = statusBytes(t, model.Status{
		Machine:    "work1",
		LocalUsers: []string{"bob"},
		LastSync:   time.Now().UTC().Format(time.RFC3339),
	})

	states, err := m.CollectMachines(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected 1 machine, got %d", len(states))
	}
	if !states[0].Unbound {
		t.Error("machine with a report but no binding should be Unbound")
	}
	if states[0].Bound {
		t.Error("Bound should be false when there is no binding")
	}
	if states[0].UserMissing {
		t.Error("UserMissing is meaningless with no binding, should be false")
	}
}

// The employee assigned to the machine has no profile on it — a sign the
// binding is wrong, or that the machine has not been logged into yet.
func TestCollectMachinesUserMissing(t *testing.T) {
	m, store := newManager()
	if err := m.AddUser("alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	store.objects[ossclient.StatusKey("work1")] = statusBytes(t, model.Status{
		Machine:    "work1",
		LocalUsers: []string{"someoneelse"},
		LastSync:   time.Now().UTC().Format(time.RFC3339),
	})

	states, err := m.CollectMachines(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !states[0].UserMissing {
		t.Error("bound employee alice has no profile on work1, should be UserMissing")
	}
}

// Local profiles that are neither the bound employee nor Administrator are
// unexpected accounts the admin should know about.
func TestCollectMachinesExtraUsers(t *testing.T) {
	m, store := newManager()
	if err := m.AddUser("alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	store.objects[ossclient.StatusKey("work1")] = statusBytes(t, model.Status{
		Machine:    "work1",
		LocalUsers: []string{"alice", "Administrator", "mystery-guest"},
		LastSync:   time.Now().UTC().Format(time.RFC3339),
	})

	states, err := m.CollectMachines(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(states[0].ExtraUsers) != 1 || states[0].ExtraUsers[0] != "mystery-guest" {
		t.Errorf("ExtraUsers = %v, want [mystery-guest]", states[0].ExtraUsers)
	}
}

// Results must be stable regardless of write order, since the admin UI/CLI
// renders them directly.
func TestCollectMachinesSortedByHostname(t *testing.T) {
	m, store := newManager()
	if err := m.AddUser("alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("zebra", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("apple", "alice", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	store.objects[ossclient.StatusKey("mango")] = statusBytes(t, model.Status{Machine: "mango", LastSync: now})

	states, err := m.CollectMachines(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 3 {
		t.Fatalf("expected 3 machines, got %d", len(states))
	}
	got := []string{states[0].Machine, states[1].Machine, states[2].Machine}
	want := []string{"apple", "mango", "zebra"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("machines = %v, want %v", got, want)
			break
		}
	}
}

// A machine still bound to an offboarded employee is a loose end: it is
// running, reporting, and nobody owns it. It must stay visible rather than
// vanish from the view.
func TestCollectMachinesFlagsDisabledUsers(t *testing.T) {
	m, store := newManager()
	if err := m.AddUser("work1", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("DESKTOP-A", "work1", ""); err != nil {
		t.Fatal(err)
	}
	store.objects[ossclient.StatusKey("DESKTOP-A")] = statusBytes(t, model.Status{
		Machine:    "DESKTOP-A",
		BoundUser:  "work1",
		LocalUsers: []string{"Administrator", "work1"},
		LastSync:   time.Now().UTC().Format(time.RFC3339),
	})
	if err := m.SetUserEnabled("work1", false); err != nil {
		t.Fatal(err)
	}

	states, err := m.CollectMachines(2 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d machines, want 1 -- a machine bound to a disabled user must not disappear", len(states))
	}
	if !states[0].Disabled {
		t.Error("the machine should be flagged as bound to a disabled user")
	}
}
