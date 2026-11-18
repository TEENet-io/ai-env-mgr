package agentcore

import (
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

type instructionsRecorder struct {
	calls map[string]bool
	fail  string
}

func (r *instructionsRecorder) Sync(profileDir, user string, enabled bool) (bool, error) {
	if r.calls == nil {
		r.calls = map[string]bool{}
	}
	r.calls[user] = enabled
	if user == r.fail {
		return false, errors.New("disk full")
	}
	return true, nil
}

func TestInstructionsFollowTheBindingAndNeverStopTheSync(t *testing.T) {
	store := newFakeStore()
	s := newSyncer(t, store, &fakeApplier{})
	s.Machine.(*fakeMachine).localUsers = []string{"Administrator", "peter", "weipeng"}
	store.set(ossclient.PolicyKey(), policyBytes(t, model.DefaultPolicy()), "petag")
	rec := &instructionsRecorder{fail: "weipeng"}
	s.Instructions = rec

	bind(t, store, "DESKTOP-A", "peter")
	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 2 || rec.calls["peter"] != true || rec.calls["weipeng"] != false {
		t.Fatalf("bound calls = %v; want peter on, weipeng off, Administrator untouched", rec.calls)
	}
	if !strings.Contains(strings.Join(st.Warnings, "\n"), `codex instructions for "weipeng": disk full`) {
		t.Fatalf("a failed profile must be reported, got %v", st.Warnings)
	}

	delete(store.objects, ossclient.BindingKey("DESKTOP-A"))
	delete(store.etags, ossclient.BindingKey("DESKTOP-A"))
	rec.calls, rec.fail = nil, ""
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"peter", "weipeng"} {
		if enabled, ok := rec.calls[user]; !ok || enabled {
			t.Fatalf("unbound calls = %v; want every profile off", rec.calls)
		}
	}
}
