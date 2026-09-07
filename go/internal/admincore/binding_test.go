package admincore

import "testing"

func TestBindMachineThenLoad(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "", "")
	if err := m.BindMachine("work1", "alice", "reimaged 2026-08"); err != nil {
		t.Fatal(err)
	}

	b, ok, err := m.LoadBinding("work1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected binding to be found")
	}
	if b.User != "alice" {
		t.Errorf("User = %q, want alice", b.User)
	}
	if b.Note != "reimaged 2026-08" {
		t.Errorf("Note = %q, want %q", b.Note, "reimaged 2026-08")
	}
	if b.BoundAt == "" {
		t.Error("BoundAt should be stamped")
	}
}

// Binding to a user not on the roster would hand a machine to nobody, and
// typos would only surface once the agent came up empty-handed.
func TestBindMachineRejectsUnknownUser(t *testing.T) {
	m, _ := newManager()
	if err := m.BindMachine("work1", "ghost", ""); err == nil {
		t.Fatal("expected error for a user not in the roster")
	}
}

// Rebinding a machine (reassignment, fixing a typo) must overwrite the
// previous binding rather than erroring or duplicating it.
func TestBindMachineIsIdempotent(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "", "")
	addTestUser(t, m, "bob", "", "")
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("work1", "bob", "reassigned"); err != nil {
		t.Fatal(err)
	}

	b, ok, err := m.LoadBinding("work1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected binding to be found")
	}
	if b.User != "bob" {
		t.Errorf("User = %q, want bob (rebinding should overwrite)", b.User)
	}
}

func TestLoadBindingMissing(t *testing.T) {
	m, _ := newManager()
	b, ok, err := m.LoadBinding("nowhere")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("expected no binding, got %+v", b)
	}
}

func TestUnbindMachine(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "", "")
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.UnbindMachine("work1"); err != nil {
		t.Fatal(err)
	}

	_, ok, err := m.LoadBinding("work1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected no binding after UnbindMachine")
	}
}

// Unbinding a machine with no binding is a no-op, not an error: the admin
// might double-click, or unbind a machine that never got bound.
func TestUnbindMachineNotBoundIsNoop(t *testing.T) {
	m, _ := newManager()
	if err := m.UnbindMachine("never-bound"); err != nil {
		t.Fatalf("unbinding a never-bound machine should not error: %v", err)
	}
}

func TestListBindings(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "", "")
	addTestUser(t, m, "bob", "", "")
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("work2", "bob", ""); err != nil {
		t.Fatal(err)
	}

	bindings, err := m.ListBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d: %+v", len(bindings), bindings)
	}
	if bindings["work1"].User != "alice" {
		t.Errorf("work1 -> %q, want alice", bindings["work1"].User)
	}
	if bindings["work2"].User != "bob" {
		t.Errorf("work2 -> %q, want bob", bindings["work2"].User)
	}
}
