package admincore

import (
	"fmt"

	"github.com/TEENet-io/airlock/internal/creds"
	"github.com/TEENet-io/airlock/internal/model"
)

// PublishCredentials merges set into the user's existing credential archive
// and uploads the result.
//
// The user must already be on the roster: their directory is derived from
// the Windows user name, and publishing to an unknown name would just
// scatter an object under an arbitrary key instead of reaching a machine.
//
// The merge matters because Codex and Claude credentials are published
// independently (e.g. re-authenticating just one tool); overwriting the
// archive outright would silently drop whichever tool's credentials were
// not part of this call.
func (m *Manager) PublishCredentials(windowsUser string, set model.CredentialSet) error {
	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	if us.Find(windowsUser) == nil {
		return fmt.Errorf("user %q not found in roster", windowsUser)
	}

	existing := model.CredentialSet{}
	if data, _, err := m.Store.Get(credsKey(windowsUser)); err == nil {
		if unpacked, err := creds.Unpack(data); err == nil {
			existing = unpacked
		}
	}
	merged := creds.Merge(existing, set)

	blob, err := creds.Pack(merged)
	if err != nil {
		return fmt.Errorf("pack credentials for %q: %w", windowsUser, err)
	}
	if err := m.Store.Put(credsKey(windowsUser), blob); err != nil {
		return fmt.Errorf("publish credentials for %q: %w", windowsUser, err)
	}
	return nil
}
