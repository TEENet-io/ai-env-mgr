package admincore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// PublishCodexUpdate stages a repackaged Codex installer and points the fleet
// at it.
//
// It uploads the installer to an agent-readable, VERSIONED key, records the
// version, checksum and key in the shared policy, and returns the hex
// checksum. Each agent then compares the version recorded on its machine
// against the policy and, when they differ, downloads the installer, verifies
// this SHA-256, and only then installs it silently.
//
// Publishing reaches every machine. There used to be a staged rollout, sized
// as a percentage of the fleet, on the reasoning that a green CI run proves
// only that the patches applied -- not that the ChatGPT surfaces are gone or
// that Computer Use still works. That reasoning holds, but the staging did not
// serve it: on a fleet this size a small ring usually selects nobody, and the
// acceptance it was meant to buy has to happen on a real Windows machine
// before publishing anyway. Accept the build first; publishing is the last
// step, not the test.
//
// Past versions are left in place, so rolling back is publishing the previous
// version again -- no 700 MB re-upload.
func (m *Manager) PublishCodexUpdate(version string, installer []byte) (string, error) {
	if version == "" {
		return "", fmt.Errorf("a version is required")
	}
	if len(installer) == 0 {
		return "", fmt.Errorf("the Codex installer is empty")
	}
	sum := sha256.Sum256(installer)
	hexsum := hex.EncodeToString(sum[:])

	key := ossclient.CodexInstallerKey(version)
	if err := m.Store.Put(key, installer); err != nil {
		return "", fmt.Errorf("upload Codex installer: %w", err)
	}
	p, err := m.CurrentPolicy()
	if err != nil {
		return "", err
	}
	p.CodexVersion = version
	p.CodexSHA256 = hexsum
	p.CodexKey = key
	// Agents 1.2.5 through 1.2.7 gate the install on this and treat a missing
	// or zero value as "not my turn", so publishing to everyone means saying
	// 100 rather than saying nothing. Newer agents ignore it entirely.
	p.CodexRolloutPct = 100
	if _, err := m.publishPolicy(p); err != nil {
		return "", err
	}
	return hexsum, nil
}

// CancelCodexUpdate clears the target -- the kill switch. Machines that have
// not installed it yet stop trying; ones that already did stay as they are, so
// this halts a bad rollout but does not undo it. To actually go back, publish
// the previous version again (its installer is still in OSS).
func (m *Manager) CancelCodexUpdate() error {
	p, err := m.CurrentPolicy()
	if err != nil {
		return err
	}
	p.CodexVersion = ""
	p.CodexSHA256 = ""
	p.CodexKey = ""
	p.CodexRolloutPct = 0
	_, err = m.publishPolicy(p)
	return err
}
