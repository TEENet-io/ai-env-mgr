package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// fakeCodex records what was asked of it and answers as configured.
type fakeCodex struct {
	installed  string
	running    bool
	free       uint64
	installErr error

	installedPath    string
	installedVersion string
	installs         int
}

func (f *fakeCodex) InstalledVersion() (string, error) { return f.installed, nil }
func (f *fakeCodex) Running() (bool, error)            { return f.running, nil }
func (f *fakeCodex) FreeBytes() (uint64, error)        { return f.free, nil }
func (f *fakeCodex) Install(path, version string) error {
	f.installs++
	f.installedPath, f.installedVersion = path, version
	if f.installErr != nil {
		return f.installErr
	}
	f.installed = version
	return nil
}

func newCodexSyncer(t *testing.T, store *fakeStore, codex *fakeCodex) *Syncer {
	t.Helper()
	s := newSyncer(t, store, &fakeApplier{})
	s.Codex = codex
	return s
}

// codexPolicy publishes an installer and returns the policy pointing at it.
func codexPolicy(store *fakeStore, version string, payload []byte, pct int) model.Policy {
	key := "agent_workdir/_codex/codex-setup-" + version + ".exe"
	store.objects[key] = payload
	sum := sha256.Sum256(payload)
	return model.Policy{
		CodexVersion:    version,
		CodexKey:        key,
		CodexSHA256:     hex.EncodeToString(sum[:]),
		CodexRolloutPct: pct,
	}
}

func TestCodexInstallsWhenEligible(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{free: 100 << 30}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "26.810.4967.0+b7", []byte("setup"), 100)

	var errs []string
	got, state := s.updateCodex(pol, &errs)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if codex.installs != 1 || codex.installedVersion != pol.CodexVersion {
		t.Fatalf("installs=%d version=%q", codex.installs, codex.installedVersion)
	}
	if got != pol.CodexVersion || state != CodexIdle {
		t.Fatalf("version=%q state=%q", got, state)
	}
	// The 700 MB download is not left behind.
	if _, err := os.Stat(filepath.Join(s.StateDir, "codex-setup.exe")); !os.IsNotExist(err) {
		t.Fatal("the installer was left on disk")
	}
}

// The checksum is what stands between the fleet and running whatever happens
// to be at that key, so a mismatch must not install and must not retry.
func TestCodexRefusesAChecksumMismatch(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{free: 100 << 30}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "1.0.0", []byte("setup"), 100)
	pol.CodexSHA256 = strings.Repeat("00", 32)

	var errs []string
	_, state := s.updateCodex(pol, &errs)
	if codex.installs != 0 {
		t.Fatal("installed despite a checksum mismatch")
	}
	if state != CodexFailed {
		t.Fatalf("state = %q, want failed", state)
	}
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), "checksum mismatch") {
		t.Fatalf("the mismatch was not reported: %v", errs)
	}
	if _, err := os.Stat(filepath.Join(s.StateDir, "codex-setup.exe")); !os.IsNotExist(err) {
		t.Fatal("the rejected download was kept")
	}
}

// Equality, not "newer": publishing an older version is how a bad build is
// rolled back.
func TestCodexInstallsAnOlderPublishedVersion(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{installed: "2.0.0", free: 100 << 30}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "1.0.0", []byte("older"), 100)

	var errs []string
	if _, state := s.updateCodex(pol, &errs); state != CodexIdle {
		t.Fatalf("state = %q", state)
	}
	if codex.installedVersion != "1.0.0" {
		t.Fatalf("did not roll back: %q", codex.installedVersion)
	}
}

func TestCodexSkipsWhenAlreadyOnTheTarget(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{installed: "1.0.0", free: 100 << 30}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "1.0.0", []byte("setup"), 100)

	var errs []string
	s.updateCodex(pol, &errs)
	if codex.installs != 0 {
		t.Fatal("reinstalled a version already present")
	}
}

// An empty version is the kill switch.
func TestCodexDoesNothingWithoutATarget(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{free: 100 << 30}
	s := newCodexSyncer(t, store, codex)

	var errs []string
	if _, state := s.updateCodex(model.Policy{}, &errs); state != CodexIdle {
		t.Fatalf("state = %q", state)
	}
	if codex.installs != 0 || len(errs) > 0 {
		t.Fatalf("acted with no target: installs=%d errs=%v", codex.installs, errs)
	}
}

// Installing over a running Codex fails on locked files and would yank the
// application away from whoever is using it. Waiting is not an error.
func TestCodexDefersWhileInUse(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{running: true, free: 100 << 30}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "1.0.0", []byte("setup"), 100)

	var errs []string
	_, state := s.updateCodex(pol, &errs)
	if codex.installs != 0 {
		t.Fatal("installed while Codex was running")
	}
	if state != CodexDeferred {
		t.Fatalf("state = %q, want deferred", state)
	}
	if len(errs) > 0 {
		t.Fatalf("deferring reported an error: %v", errs)
	}
}

func TestCodexDefersWhenTheDiskIsFull(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{free: 100 << 20} // 100 MB
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "1.0.0", []byte("setup"), 100)

	var errs []string
	_, state := s.updateCodex(pol, &errs)
	if codex.installs != 0 || state != CodexDeferred {
		t.Fatalf("installs=%d state=%q", codex.installs, state)
	}
}

// A package that fails must not be re-downloaded every sync on every machine
// in the ring.
func TestCodexTriesAFailedVersionOnce(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{free: 100 << 30, installErr: os.ErrPermission}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "1.0.0", []byte("setup"), 100)

	var errs []string
	s.updateCodex(pol, &errs)
	s.updateCodex(pol, &errs)
	if codex.installs != 1 {
		t.Fatalf("attempted %d times, want 1", codex.installs)
	}

	// A newly published version is attempted again.
	pol2 := codexPolicy(store, "1.0.1", []byte("setup2"), 100)
	s.updateCodex(pol2, &errs)
	if codex.installs != 2 {
		t.Fatalf("a new version was not attempted: installs=%d", codex.installs)
	}
}

// The ring has to be stable: widening it adds machines rather than reshuffling
// which ones are exposed.
func TestRolloutIsStableAndProportional(t *testing.T) {
	if !inRollout("any", 100) {
		t.Fatal("100% excluded a machine")
	}
	if inRollout("any", 0) {
		t.Fatal("0% included a machine")
	}
	// Case does not change a machine's ring: Windows hostnames vary in case.
	if inRollout("DESKTOP-A", 50) != inRollout("desktop-a", 50) {
		t.Fatal("the ring depends on hostname case")
	}
	// A machine in the first 10% stays in every wider ring.
	var first []string
	for _, m := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		if inRollout(m, 10) {
			first = append(first, m)
		}
	}
	for _, m := range first {
		for _, pct := range []int{10, 25, 50, 100} {
			if !inRollout(m, pct) {
				t.Fatalf("%q left the ring at %d%%", m, pct)
			}
		}
	}
}
