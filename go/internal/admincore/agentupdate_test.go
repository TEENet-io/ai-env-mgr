package admincore

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func TestPublishAgentUpdateUploadsAndSetsPolicy(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	bin := []byte("new agent binary v1.2.0")
	want := sha256.Sum256(bin)

	got, err := m.PublishAgentUpdate("1.2.0", bin)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("returned sha %s, want %s", got, hex.EncodeToString(want[:]))
	}
	if string(fs.objects[ossclient.AgentBinaryKey()]) != string(bin) {
		t.Fatal("binary was not uploaded to the agent-readable key")
	}
	p, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.AgentUpdateVersion != "1.2.0" || p.AgentUpdateSHA256 != got {
		t.Fatalf("policy not set: version=%q sha=%q", p.AgentUpdateVersion, p.AgentUpdateSHA256)
	}
}

func TestCancelAgentUpdateClearsTarget(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	if _, err := m.PublishAgentUpdate("1.2.0", []byte("bin")); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelAgentUpdate(); err != nil {
		t.Fatal(err)
	}
	p, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.AgentUpdateVersion != "" || p.AgentUpdateSHA256 != "" {
		t.Fatalf("cancel did not clear the target: %q %q", p.AgentUpdateVersion, p.AgentUpdateSHA256)
	}
}

func TestPublishAgentUpdateRejectsEmpty(t *testing.T) {
	m := &Manager{Store: newFakeStore()}
	if _, err := m.PublishAgentUpdate("", []byte("bin")); err == nil {
		t.Fatal("empty version should be rejected")
	}
	if _, err := m.PublishAgentUpdate("1.0.0", nil); err == nil {
		t.Fatal("empty binary should be rejected")
	}
}
