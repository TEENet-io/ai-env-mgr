package admincore

import (
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// FetchLog returns the recent agent log a machine uploaded, so the admin can
// read it without reaching the machine. A missing object means the agent has
// not uploaded one yet (or lacks the _logs/ write permission).
func (m *Manager) FetchLog(machine string) ([]byte, error) {
	data, _, err := m.Store.Get(ossclient.LogKey(machine))
	if err != nil {
		return nil, fmt.Errorf("no log for %q yet: %w", machine, err)
	}
	return data, nil
}
