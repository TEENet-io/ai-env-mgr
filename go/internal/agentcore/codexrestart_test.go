package agentcore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// bindWithRestart writes a binding carrying a one-shot restart request.
func bindWithRestart(t *testing.T, store *fakeStore, machine, user, nonce string) {
	t.Helper()
	out, err := json.Marshal(model.Binding{
		User: user, BoundAt: "now",
		RestartCodex:   nonce,
		RestartCodexAt: "2026-09-06T08:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.set(ossclient.BindingKey(machine), out, "b-"+nonce)
}

// restartSyncer is a machine bound to work1 with a plain policy and no
// credentials object, so the only thing under test is the request itself.
func restartSyncer(t *testing.T, app *fakeApplier) (*Syncer, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")
	return newSyncer(t, store, app), store
}

// The request is one-shot: the agent re-reads the same binding every cycle,
// and a second kill would take work the employee had started since.
func TestRestartRequestActsOncePerNonce(t *testing.T) {
	app := &fakeApplier{stopKilled: 1}
	s, store := restartSyncer(t, app)
	bindWithRestart(t, store, "DESKTOP-A", "work1", "nonce-1")

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(app.stoppedFor, []string{"work1"}) {
		t.Fatalf("stopped %v, want one restart for work1", app.stoppedFor)
	}
	if st.CodexRestartNonce != "nonce-1" {
		t.Errorf("CodexRestartNonce = %q, want the nonce that was acted on", st.CodexRestartNonce)
	}
	if st.CodexRestartNote != "killed 1 process" {
		t.Errorf("CodexRestartNote = %q, want %q", st.CodexRestartNote, "killed 1 process")
	}
	if st.CodexRestartAt == "" {
		t.Error("CodexRestartAt should say when the agent acted")
	}

	// The same binding, several more cycles: nothing further may happen, and
	// the recorded outcome must still be reported or the console would show
	// the request as forever pending.
	for i := 0; i < 3; i++ {
		st, err = s.RunOnce()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(app.stoppedFor) != 1 {
		t.Fatalf("the same nonce acted on %d times, want 1", len(app.stoppedFor))
	}
	if st.CodexRestartNonce != "nonce-1" || st.CodexRestartNote != "killed 1 process" {
		t.Errorf("the outcome must keep being reported, got %q / %q",
			st.CodexRestartNonce, st.CodexRestartNote)
	}

	// A fresh nonce is a fresh request.
	bindWithRestart(t, store, "DESKTOP-A", "work1", "nonce-2")
	st, err = s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(app.stoppedFor, []string{"work1", "work1"}) {
		t.Errorf("a new nonce stopped %v, want a second restart", app.stoppedFor)
	}
	if st.CodexRestartNonce != "nonce-2" {
		t.Errorf("CodexRestartNonce = %q, want nonce-2", st.CodexRestartNonce)
	}
}

// A cycle that both delivers changed credentials and finds a pending request
// has one session to end, not two. Killing twice would take whatever the
// employee reopened in the seconds between -- and from their side the second
// one is inexplicable, since nothing happened in between that they could see.
func TestDeliveryAndRequestInOneCycleInterruptOnce(t *testing.T) {
	app := &fakeApplier{stopKilled: 2}
	s, store := restartSyncer(t, app)
	bindWithRestart(t, store, "DESKTOP-A", "work1", "nonce-1")
	store.set(ossclient.UserKey("work1", "credentials.zip"), credsBytesFor(t, model.CredentialSet{
		model.PathCodexAuth: []byte(`{"token":"t1"}`),
	}), "c1")

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(app.stoppedFor) != 1 {
		t.Fatalf("the session was ended %d times in one cycle, want 1: %v", len(app.stoppedFor), app.stoppedFor)
	}
	// The request still counts as carried out, with the sweep's outcome, or
	// the console would show it pending forever.
	if st.CodexRestartNonce != "nonce-1" {
		t.Errorf("CodexRestartNonce = %q, want nonce-1", st.CodexRestartNonce)
	}
	if st.CodexRestartNote != "killed 2 processes" {
		t.Errorf("CodexRestartNote = %q, want the sweep's own result", st.CodexRestartNote)
	}

	// And it does not come back on the next cycle: the marker was written.
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(app.stoppedFor) != 1 {
		t.Errorf("the request ran again after being covered: %v", app.stoppedFor)
	}
}

// A failed sweep is the request's outcome too: reporting "killed 0" for a
// taskkill that was refused would tell the administrator the session is
// clean when it is not.
func TestARequestCoveredByAFailedSweepReportsTheFailure(t *testing.T) {
	app := &fakeApplier{stopErr: errors.New("access is denied")}
	s, store := restartSyncer(t, app)
	bindWithRestart(t, store, "DESKTOP-A", "work1", "nonce-1")
	store.set(ossclient.UserKey("work1", "credentials.zip"), credsBytesFor(t, model.CredentialSet{
		model.PathCodexAuth: []byte(`{"token":"t1"}`),
	}), "c1")

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(app.stoppedFor) != 1 {
		t.Fatalf("stopped %d times, want 1", len(app.stoppedFor))
	}
	if !strings.Contains(st.CodexRestartNote, "access is denied") {
		t.Errorf("CodexRestartNote = %q, want the failure", st.CodexRestartNote)
	}
}

func TestRestartRequestWritesTheNonceMarker(t *testing.T) {
	app := &fakeApplier{stopKilled: 1}
	s, store := restartSyncer(t, app)
	bindWithRestart(t, store, "DESKTOP-A", "work1", "nonce-1")

	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.StateDir, codexRestartMarkerFile))
	if err != nil {
		t.Fatalf("the acted-on nonce must be on disk, or a restart would repeat: %v", err)
	}
	if !strings.Contains(string(raw), "nonce-1") {
		t.Errorf("marker = %s, want it to carry nonce-1", raw)
	}
}

// A binding with no request must not restart anything. This is every machine
// in the fleet on every ordinary cycle.
func TestNoRestartRequestDoesNothing(t *testing.T) {
	app := &fakeApplier{}
	s, store := restartSyncer(t, app)
	bind(t, store, "DESKTOP-A", "work1")

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(app.stoppedFor) != 0 {
		t.Errorf("nobody asked for a restart: %v", app.stoppedFor)
	}
	if st.CodexRestartNonce != "" || st.CodexRestartNote != "" {
		t.Errorf("an unasked machine reported an outcome: %q / %q",
			st.CodexRestartNonce, st.CodexRestartNote)
	}
}

// A machine bound to somebody who has never signed in has no session to end.
// The request stays pending -- it should run once the profile appears --
// which is why no nonce is reported.
func TestRestartRequestWithoutAProfileIsExplained(t *testing.T) {
	app := &fakeApplier{}
	s, store := restartSyncer(t, app)
	bindWithRestart(t, store, "DESKTOP-A", "ghost", "nonce-1")

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(app.stoppedFor) != 0 {
		t.Errorf("there is no profile to act on: %v", app.stoppedFor)
	}
	if st.CodexRestartNote != noBoundUserProfileNote {
		t.Errorf("CodexRestartNote = %q, want %q", st.CodexRestartNote, noBoundUserProfileNote)
	}
	if st.CodexRestartNonce != "" {
		t.Errorf("nothing was acted on, so no nonce may be claimed: %q", st.CodexRestartNonce)
	}
}

// A kill that cannot succeed on this machine must not be retried on every
// cycle: each retry is another attempt to take somebody's session away, and a
// request repeating forever is worse than one that failed visibly.
func TestFailedRestartRequestIsMarkedAndNotRetried(t *testing.T) {
	app := &fakeApplier{stopErr: errors.New("access is denied")}
	s, store := restartSyncer(t, app)
	bindWithRestart(t, store, "DESKTOP-A", "work1", "nonce-1")

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("a failed restart must not fail the sync: %v", err)
	}
	if st.CodexRestartNonce != "nonce-1" {
		t.Errorf("a failed attempt still counts as attempted: %q", st.CodexRestartNonce)
	}
	if !strings.Contains(st.CodexRestartNote, "access is denied") {
		t.Errorf("CodexRestartNote = %q, want the error in it", st.CodexRestartNote)
	}
	if len(st.Errors) != 0 {
		t.Errorf("a failed restart is a warning, not a cycle error: %v", st.Errors)
	}
	if !hasSubstring(st.Warnings, "FAILED") {
		t.Errorf("the failure should be visible in status, got: %v", st.Warnings)
	}

	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(app.stoppedFor) != 1 {
		t.Errorf("a failed request was retried %d times; the marker should stop that", len(app.stoppedFor))
	}
}

// A binding naming something that is not an account -- `*`, say, which as a
// taskkill filter would match every session on the box -- never reaches the
// kill at all: no local profile matches it, so the request stops at the same
// check that catches an employee who has not signed in yet. The name is
// validated again further down (creds.stopCodexArgs), but this is the layer
// that means a bad name in the store cannot even get that far.
func TestRestartRequestNamingSomethingThatIsNotAnAccountNeverKills(t *testing.T) {
	app := &fakeApplier{stopKilled: 1}
	s, store := restartSyncer(t, app)
	bindWithRestart(t, store, "DESKTOP-A", "*", "nonce-1")

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(app.stoppedFor) != 0 {
		t.Fatalf("a wildcard binding reached the kill: %v", app.stoppedFor)
	}
	if st.CodexRestartNote != noBoundUserProfileNote {
		t.Errorf("CodexRestartNote = %q, want %q", st.CodexRestartNote, noBoundUserProfileNote)
	}
}

// Nothing running is the common case -- most people do not have Codex open
// when an administrator presses the button -- and it is a success.
func TestRestartRequestWithNothingRunningSaysSo(t *testing.T) {
	app := &fakeApplier{stopKilled: 0}
	s, store := restartSyncer(t, app)
	bindWithRestart(t, store, "DESKTOP-A", "work1", "nonce-1")

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if st.CodexRestartNote != "no process" {
		t.Errorf("CodexRestartNote = %q, want %q", st.CodexRestartNote, "no process")
	}
	if len(st.Errors) != 0 {
		t.Errorf("an empty session is not an error: %v", st.Errors)
	}
}

func TestKilledNote(t *testing.T) {
	cases := []struct {
		killed int
		want   string
	}{
		{0, "no process"},
		{1, "killed 1 process"},
		{3, "killed 3 processes"},
	}
	for _, c := range cases {
		if got := killedNote(c.killed); got != c.want {
			t.Errorf("killedNote(%d) = %q, want %q", c.killed, got, c.want)
		}
	}
}
