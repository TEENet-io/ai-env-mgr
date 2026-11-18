package agentcore

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

type memorySyncCall struct {
	profile string
	enabled bool
}

type fakeManagedMemoryCodex struct {
	*fakeCodex
	calls []memorySyncCall
	err   error
}

func (f *fakeManagedMemoryCodex) SyncManagedMemory(profileDir string, enabled bool) (bool, error) {
	f.calls = append(f.calls, memorySyncCall{profileDir, enabled})
	return true, f.err
}

func TestManagedMemoryTracksBindingWithoutTouchingAdministrator(t *testing.T) {
	store := newFakeStore()
	s := newSyncer(t, store, &fakeApplier{})
	codex := &fakeManagedMemoryCodex{fakeCodex: &fakeCodex{installed: "1.0"}}
	s.Codex = codex
	bind(t, store, "DESKTOP-A", "work1")
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(codex.calls) != 1 || !codex.calls[0].enabled || filepath.Base(codex.calls[0].profile) != "work1" {
		t.Fatalf("bound profile was not enabled: %+v", codex.calls)
	}

	delete(store.objects, ossclient.BindingKey("DESKTOP-A"))
	delete(store.etags, ossclient.BindingKey("DESKTOP-A"))
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(codex.calls) != 2 || codex.calls[1].enabled || filepath.Base(codex.calls[1].profile) != "work1" {
		t.Fatalf("unbound profile was not disabled: %+v", codex.calls)
	}
}

func TestManagedMemoryFailureAppearsInStatus(t *testing.T) {
	store := newFakeStore()
	s := newSyncer(t, store, &fakeApplier{})
	s.Codex = &fakeManagedMemoryCodex{fakeCodex: &fakeCodex{installed: "1.0"}, err: errNotFound}
	bind(t, store, "DESKTOP-A", "work1")
	status, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(status.Errors, "\n"), "personal memory sync") {
		t.Fatalf("memory distribution failure was hidden: %+v", status.Errors)
	}
}
