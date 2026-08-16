package admincore

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func TestPublishCodexUpdateUploadsAndSetsPolicy(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	installer := []byte("codex setup 26.810.4967.0+b7")
	want := sha256.Sum256(installer)

	got, err := m.PublishCodexUpdate("26.810.52044-b1", installer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("returned sha %s, want %s", got, hex.EncodeToString(want[:]))
	}
	key := ossclient.CodexInstallerKey("26.810.52044-b1")
	if string(fs.objects[key]) != string(installer) {
		t.Fatalf("installer was not uploaded to %s", key)
	}
	p, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.CodexVersion != "26.810.52044-b1" || p.CodexSHA256 != got {
		t.Fatalf("policy not set: version=%q sha=%q", p.CodexVersion, p.CodexSHA256)
	}
	if p.CodexKey != key {
		t.Fatalf("policy key = %q, want %q", p.CodexKey, key)
	}
	// Agents 1.2.5-1.2.7 skip the install unless this says so, so publishing
	// to the whole fleet means writing 100 rather than leaving it unset.
	if p.CodexRolloutPct != 100 {
		t.Fatalf("rollout = %d, want 100 for agents that still gate on it", p.CodexRolloutPct)
	}
}

// Rolling back must not need a 700 MB re-upload, so publishing a second
// version has to leave the first installer in place.
func TestPublishCodexUpdateKeepsPreviousVersions(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	if _, err := m.PublishCodexUpdate("1.0.0", []byte("old"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PublishCodexUpdate("2.0.0", []byte("new"), nil); err != nil {
		t.Fatal(err)
	}
	if string(fs.objects[ossclient.CodexInstallerKey("1.0.0")]) != "old" {
		t.Fatal("publishing a new version dropped the previous installer; rollback would need a re-upload")
	}
	p, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.CodexVersion != "2.0.0" {
		t.Fatalf("target version = %q, want 2.0.0", p.CodexVersion)
	}
}

func TestCancelCodexUpdateClearsTarget(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	if _, err := m.PublishCodexUpdate("1.0.0", []byte("bin"), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelCodexUpdate(); err != nil {
		t.Fatal(err)
	}
	p, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.CodexVersion != "" || p.CodexSHA256 != "" || p.CodexKey != "" || p.CodexRolloutPct != 0 {
		t.Fatalf("target not cleared: %+v", p)
	}
	// The installer stays put: cancelling stops further installs, and the
	// object is what a rollback republishes.
	if string(fs.objects[ossclient.CodexInstallerKey("1.0.0")]) != "bin" {
		t.Fatal("cancel deleted the installer")
	}
}

func TestPublishCodexUpdateRejectsBadInput(t *testing.T) {
	m := &Manager{Store: newFakeStore()}
	if _, err := m.PublishCodexUpdate("", []byte("bin"), nil); err == nil {
		t.Fatal("empty version was accepted")
	}
	if _, err := m.PublishCodexUpdate("1.0.0", nil, nil); err == nil {
		t.Fatal("empty installer was accepted")
	}
}

func TestCodexInstallerKeyIsVersionedAndSanitised(t *testing.T) {
	a := ossclient.CodexInstallerKey("26.810.52044-b1")
	b := ossclient.CodexInstallerKey("26.803.10989.0+b2")
	if a == b {
		t.Fatal("different versions share a key; publishing one would overwrite the other")
	}
	// A version string reaches this from a command line, so it must not be
	// able to escape the prefix.
	if got := ossclient.CodexInstallerKey("../../etc/passwd"); !hasPrefix(got, ossclient.CodexPrefix) {
		t.Fatalf("key %q escaped %q", got, ossclient.CodexPrefix)
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
