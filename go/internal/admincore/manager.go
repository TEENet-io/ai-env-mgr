// Package admincore implements the admin-side business logic: maintaining
// the user roster, publishing policy and credentials, and collecting
// machine status.
//
// It is deliberately free of OSS and CLI specifics: the object store arrives
// as an interface, so the whole surface can be exercised with an in-memory
// fake, the same approach agentcore uses for the machine side.
package admincore

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// Store is the subset of the object store the admin needs.
//
// List and Delete exist for machine bindings: the admin needs to enumerate
// every _bindings/ and _status/ object to build the machine-centric view,
// and to remove a binding object outright when unbinding a machine (an
// empty/absent binding, not an empty file, is what "unbound" means).
type Store interface {
	Get(key string) ([]byte, string, error)
	Put(key string, data []byte) error
	List(prefix string) ([]string, error)
	// ListInfo is List with per-object size and modification time, used to
	// summarise collected session data without downloading it.
	ListInfo(prefix string) ([]ossclient.ObjectInfo, error)
	Delete(key string) error
	// SignedURL grants temporary read access to one object without the
	// holder needing credentials -- how a staged file reaches a machine.
	SignedURL(key string, ttl time.Duration) (string, error)
}

// Manager implements the admin operations against a Store.
type Manager struct {
	Store Store
}

// Both sides must build keys the same way or the admin writes where the agent
// never looks, so these go through the shared helper rather than assembling
// paths here.
func credsKey(user string) string { return ossclient.UserKey(user, "credentials.zip") }

// LoadUsers reads the roster. A bucket with no roster yet is simply the
// admin's first run, not an error, so a missing object yields an empty one
// rather than failing.
func (m *Manager) LoadUsers() (model.Users, error) {
	data, _, err := m.Store.Get(ossclient.AdminKey("users.json"))
	if err != nil {
		return model.Users{}, nil
	}
	var us model.Users
	if err := json.Unmarshal(data, &us); err != nil {
		return model.Users{}, fmt.Errorf("parse users: %w", err)
	}
	return us, nil
}

// SaveUsers writes the roster back, stamping the update time.
func (m *Manager) SaveUsers(us model.Users) error {
	us.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(us, "", "  ")
	if err != nil {
		return fmt.Errorf("encode users: %w", err)
	}
	if err := m.Store.Put(ossclient.AdminKey("users.json"), data); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	return nil
}

// AddUser enrolls a Windows user, or updates their account labels if they
// are already on the roster. Re-provisioning a machine under the same
// Windows account is expected to happen (reimages, handovers), so this is
// idempotent rather than erroring on a duplicate.
func (m *Manager) AddUser(windowsUser, codexAccount, claudeAccount string) error {
	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	if e := us.Find(windowsUser); e != nil {
		e.CodexAccount = codexAccount
		e.ClaudeAccount = claudeAccount
		e.Enabled = true
	} else {
		us.Users = append(us.Users, model.UserEntry{
			WindowsUser:   windowsUser,
			CodexAccount:  codexAccount,
			ClaudeAccount: claudeAccount,
			Enabled:       true,
		})
	}
	return m.SaveUsers(us)
}

// SetUserEnabled flips a user's enabled flag, which is how a person is taken
// out of rotation without deleting their history.
//
// Disabling also deletes the employee's stored credentials. That deletion is
// what actually reaches the machine: an agent that gets a definitive 404 for
// its employee's credentials.zip removes the local copies. Flipping a flag in
// a roster the agent cannot even read would revoke nothing.
//
// The roster is saved first. If the delete then fails, the employee is still
// marked disabled and the next disable retries the delete; the reverse order
// could delete the credentials and then leave them marked enabled.
//
// Note this does not touch the block policy: that is machine-wide and stays
// in force regardless of who, if anyone, a machine is assigned to.
func (m *Manager) SetUserEnabled(windowsUser string, enabled bool) error {
	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	e := us.Find(windowsUser)
	if e == nil {
		return fmt.Errorf("user %q not found in roster", windowsUser)
	}
	e.Enabled = enabled
	if err := m.SaveUsers(us); err != nil {
		return err
	}

	if !enabled {
		// Use the roster's spelling, not the caller's: Find matches
		// case-insensitively, and the stored object is keyed by the former.
		if err := m.Store.Delete(credsKey(e.WindowsUser)); err != nil {
			return fmt.Errorf("user %q is disabled, but revoking their stored credentials failed: %w",
				e.WindowsUser, err)
		}
	}
	return nil
}

// CurrentPolicy returns the policy currently in effect. A bucket with no
// policy yet is the administrator's first run, not an error, so the caller
// gets the same default an agent would fall back to.
func (m *Manager) CurrentPolicy() (model.Policy, error) {
	data, _, err := m.Store.Get(ossclient.PolicyKey())
	if err != nil {
		return model.DefaultPolicy(), nil
	}
	var p model.Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return model.DefaultPolicy(), nil
	}
	return p, nil
}

// publishPolicy writes p, stamped with the current time, to the one place
// every agent reads it from.
//
// There is deliberately only one copy. The block is machine-wide, identical
// for everyone, and must apply to machines that are not assigned to anybody
// yet -- so keying it by employee would leave exactly those machines
// unprotected.
func (m *Manager) publishPolicy(p model.Policy) (model.Policy, error) {
	p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return model.Policy{}, fmt.Errorf("encode policy: %w", err)
	}
	if err := m.Store.Put(ossclient.PolicyKey(), data); err != nil {
		return p, fmt.Errorf("publish policy: %w", err)
	}
	return p, nil
}

// PublishPolicy writes p as the policy every machine will pick up.
func (m *Manager) PublishPolicy(p model.Policy) error {
	_, err := m.publishPolicy(p)
	return err
}

// normalizeDomain canonicalises a domain the same way for storage and for
// comparison, so "Foo.com" and " foo.com " count as the same entry.
func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimSpace(d))
}

// MutateDomains adds and removes blocked domains on top of the current
// policy, then publishes the result. The set is deduplicated and sorted so
// the stored list is stable regardless of call order.
func (m *Manager) MutateDomains(add, remove []string) (model.Policy, error) {
	p, err := m.CurrentPolicy()
	if err != nil {
		return model.Policy{}, err
	}

	set := make(map[string]struct{}, len(p.BlockedDomains))
	for _, d := range p.BlockedDomains {
		if n := normalizeDomain(d); n != "" {
			set[n] = struct{}{}
		}
	}
	for _, d := range add {
		if n := normalizeDomain(d); n != "" {
			set[n] = struct{}{}
		}
	}
	for _, d := range remove {
		delete(set, normalizeDomain(d))
	}

	domains := make([]string, 0, len(set))
	for d := range set {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	p.BlockedDomains = domains

	return m.publishPolicy(p)
}

// SetBlockEnabled flips the global block switch and publishes the change.
func (m *Manager) SetBlockEnabled(enabled bool) (model.Policy, error) {
	p, err := m.CurrentPolicy()
	if err != nil {
		return model.Policy{}, err
	}
	p.BlockEnabled = enabled
	return m.publishPolicy(p)
}

// SetSyncInterval changes how often agents sync and publishes the change.
//
// A bad interval can strand a machine: too short hammers the bucket, and a
// non-positive value would tell the agent to sync continuously. The value is
// always clamped to the safe range before it ever reaches a machine, using
// the current interval as the fallback for an unset (<=0) request.
func (m *Manager) SetSyncInterval(minutes int) (model.Policy, error) {
	p, err := m.CurrentPolicy()
	if err != nil {
		return model.Policy{}, err
	}
	p.SyncIntervalMinutes = model.ClampInterval(minutes, p.SyncIntervalMinutes)
	return m.publishPolicy(p)
}
