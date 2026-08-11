package admincore

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func TestForgetMachineRemovesBindingAndStatus(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	fs.objects[ossclient.BindingKey("DESKTOP-X")] = []byte(`{"user":"work1"}`)
	fs.objects[ossclient.StatusKey("DESKTOP-X")] = []byte(`{"machine":"DESKTOP-X"}`)

	if err := m.ForgetMachine("DESKTOP-X"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.objects[ossclient.BindingKey("DESKTOP-X")]; ok {
		t.Error("binding was not deleted")
	}
	if _, ok := fs.objects[ossclient.StatusKey("DESKTOP-X")]; ok {
		t.Error("status report was not deleted")
	}
}

func TestForgetMachineIsIdempotent(t *testing.T) {
	m := &Manager{Store: newFakeStore()}
	if err := m.ForgetMachine("never-existed"); err != nil {
		t.Fatalf("forgetting an unknown machine should be a no-op, got %v", err)
	}
}
