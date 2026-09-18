package admincore

import "testing"

func TestBindMachineThenLoad(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "")
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
	addTestUser(t, m, "alice", "")
	addTestUser(t, m, "bob", "")
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
	addTestUser(t, m, "alice", "")
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
	addTestUser(t, m, "alice", "")
	addTestUser(t, m, "bob", "")
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

// 统一小写: the agent keys its OSS prefix and its profile lookup off
// binding.User, so a binding must name the employee exactly as the roster
// does -- lowercase for anyone opened through the console.
func TestBindMachineStoresTheRostersSpelling(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "")
	if err := m.BindMachine("PC-1", "ALICE", ""); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := m.LoadBinding("PC-1"); b.User != "alice" {
		t.Errorf("User = %q, want alice", b.User)
	}

	// A legacy mixed-case roster entry is not rewritten, so the binding has
	// to follow it rather than lowercasing blindly.
	addTestUser(t, m, "Bob", "")
	if err := m.BindMachine("PC-2", "bob", ""); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := m.LoadBinding("PC-2"); b.User != "Bob" {
		t.Errorf("User = %q, want Bob (the roster's own spelling)", b.User)
	}
}

// The request is added to the live binding, so everything else in it has to
// survive: a button press must not quietly rewrite who the machine belongs to
// or when it was assigned.
func TestRequestCodexRestartKeepsTheRestOfTheBinding(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "")
	if err := m.BindMachine("work1", "alice", "reimaged 2026-08"); err != nil {
		t.Fatal(err)
	}
	before, _, err := m.LoadBinding("work1")
	if err != nil {
		t.Fatal(err)
	}

	if err := m.RequestCodexRestart("work1"); err != nil {
		t.Fatal(err)
	}

	after, ok, err := m.LoadBinding("work1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the binding should still be there")
	}
	if after.User != before.User || after.BoundAt != before.BoundAt || after.Note != before.Note {
		t.Errorf("the binding changed beyond the request:\n got %+v\nwas %+v", after, before)
	}
	if after.RestartCodex == "" {
		t.Fatal("no nonce was written, so the agent would never act")
	}
	if len(after.RestartCodex) != 32 {
		t.Errorf("nonce = %q, want 16 bytes of hex", after.RestartCodex)
	}
	if after.RestartCodexAt == "" {
		t.Error("RestartCodexAt should be stamped for display")
	}
}

// Two presses are two requests. A repeated nonce would be ignored by an agent
// that had already acted on it, leaving the second press silently doing
// nothing.
func TestRequestCodexRestartIssuesAFreshNonce(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "")
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.RequestCodexRestart("work1"); err != nil {
		t.Fatal(err)
	}
	first, _, _ := m.LoadBinding("work1")
	if err := m.RequestCodexRestart("work1"); err != nil {
		t.Fatal(err)
	}
	second, _, _ := m.LoadBinding("work1")
	if first.RestartCodex == second.RestartCodex {
		t.Error("the second press reused the first nonce; the agent would ignore it")
	}
}

// There is nobody to restart, and inventing a binding to carry the request
// would assign the machine to whoever the agent later found.
func TestRequestCodexRestartOnAnUnboundMachineFails(t *testing.T) {
	m, _ := newManager()
	if err := m.RequestCodexRestart("nobodys-pc"); err == nil {
		t.Fatal("expected an error for an unbound machine")
	}
}

// Rebinding is a change of hands: a request left pending belongs to the
// employee who has just been unassigned, and acting on it would end the new
// one's session for a reason that has nothing to do with them.
func TestBindMachineClearsAPendingRestartRequest(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "alice", "")
	addTestUser(t, m, "bob", "")
	if err := m.BindMachine("work1", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.RequestCodexRestart("work1"); err != nil {
		t.Fatal(err)
	}
	if err := m.BindMachine("work1", "bob", "reassigned"); err != nil {
		t.Fatal(err)
	}

	b, _, err := m.LoadBinding("work1")
	if err != nil {
		t.Fatal(err)
	}
	if b.RestartCodex != "" || b.RestartCodexAt != "" {
		t.Errorf("a new employee inherited the old one's request: %+v", b)
	}
}
