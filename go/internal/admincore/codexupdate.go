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
// Publishing does not by itself reach any machine: rolloutPct decides how much
// of the fleet is eligible. This is deliberate -- the package is a repackaging
// of an upstream build whose patches drift, and a green CI run only proves the
// patches applied, not that the ChatGPT surfaces are gone or that Computer Use
// still works. Accept a build on a real Windows machine first, then publish to
// a small ring and widen it.
//
// Past versions are left in place, so rolling back is `admin codex publish`
// against the previous version -- no 700 MB re-upload.
func (m *Manager) PublishCodexUpdate(version string, installer []byte, rolloutPct int) (string, error) {
	if version == "" {
		return "", fmt.Errorf("a version is required")
	}
	if len(installer) == 0 {
		return "", fmt.Errorf("the Codex installer is empty")
	}
	if rolloutPct < 0 || rolloutPct > 100 {
		return "", fmt.Errorf("rollout percentage must be between 0 and 100, got %d", rolloutPct)
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
	p.CodexRolloutPct = rolloutPct
	if _, err := m.publishPolicy(p); err != nil {
		return "", err
	}
	return hexsum, nil
}

// SetCodexRollout widens (or narrows) the ring without re-uploading. Narrowing
// does not uninstall anything: machines that already updated stay on the new
// version, it only stops further machines from picking it up.
func (m *Manager) SetCodexRollout(pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("rollout percentage must be between 0 and 100, got %d", pct)
	}
	p, err := m.CurrentPolicy()
	if err != nil {
		return err
	}
	if p.CodexVersion == "" {
		return fmt.Errorf("no Codex version published yet; run 'admin codex publish' first")
	}
	p.CodexRolloutPct = pct
	_, err = m.publishPolicy(p)
	return err
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
