package admincore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// PublishAgentUpdate stages a new agent binary and points the fleet at it.
//
// It uploads the binary to the agent-readable location, records the target
// version and the binary's SHA-256 in the shared policy, and returns the hex
// checksum. Every agent whose own version differs then downloads the binary,
// checks it against this SHA-256, and only then replaces itself.
//
// The policy is global, so this rolls out to every machine at once. Validate
// the binary on one machine first; there is no automatic rollback.
func (m *Manager) PublishAgentUpdate(version string, binary []byte, onProgress func(done, total int64)) (string, error) {
	if version == "" {
		return "", fmt.Errorf("a version is required")
	}
	if len(binary) == 0 {
		return "", fmt.Errorf("the agent binary is empty")
	}
	sum := sha256.Sum256(binary)
	hexsum := hex.EncodeToString(sum[:])

	if err := m.Store.Put(ossclient.AgentBinaryKey(), binary); err != nil {
		return "", fmt.Errorf("upload agent binary: %w", err)
	}
	p, err := m.CurrentPolicy()
	if err != nil {
		return "", err
	}
	p.AgentUpdateVersion = version
	p.AgentUpdateSHA256 = hexsum
	if _, err := m.publishPolicy(p); err != nil {
		return "", err
	}
	return hexsum, nil
}

// CancelAgentUpdate clears the update target -- the kill switch. Agents that
// have not updated yet stop trying; ones already updated stay put.
func (m *Manager) CancelAgentUpdate() error {
	p, err := m.CurrentPolicy()
	if err != nil {
		return err
	}
	p.AgentUpdateVersion = ""
	p.AgentUpdateSHA256 = ""
	_, err = m.publishPolicy(p)
	return err
}
