// Package model defines the data structures shared between agent and admin.
// These map 1:1 onto the JSON objects stored in OSS.
package model

import (
	"strings"
	"time"
)

// Policy is stored at agent_workdir/{user}/policy.json and drives both the browser
// blocklist and the agent's sync cadence.
type Policy struct {
	BlockEnabled        bool     `json:"blockEnabled"`
	BlockedDomains      []string `json:"blockedDomains"`
	SyncIntervalMinutes int      `json:"syncIntervalMinutes"`
	UpdatedAt           string   `json:"updatedAt"`
}

// Sync interval bounds. The interval can lock a machine out of reach if set
// badly, so it is always clamped.
const (
	MinSyncInterval     = 1
	MaxSyncInterval     = 1440
	DefaultSyncInterval = 30
)

// ClampInterval keeps a requested interval inside the safe range.
// A non-positive value means "unset" and yields fallback.
func ClampInterval(requested, fallback int) int {
	if requested <= 0 {
		requested = fallback
	}
	if requested <= 0 {
		requested = DefaultSyncInterval
	}
	if requested < MinSyncInterval {
		return MinSyncInterval
	}
	if requested > MaxSyncInterval {
		return MaxSyncInterval
	}
	return requested
}

// DefaultPolicy returns the policy used when none exists yet.
func DefaultPolicy() Policy {
	return Policy{
		BlockEnabled:        true,
		BlockedDomains:      []string{"openai.com", "chatgpt.com", "claude.ai", "anthropic.com"},
		SyncIntervalMinutes: DefaultSyncInterval,
		UpdatedAt:           time.Now().UTC().Format(time.RFC3339),
	}
}

// Binding is stored at agent_workdir/_bindings/{machine}.json and tells an agent which
// employee its machine serves.
//
// Machines are keyed by hostname because that is the only identity available
// to a service running as SYSTEM: os.UserHomeDir() would report the system
// profile, not the employee.
type Binding struct {
	User    string `json:"user"`
	BoundAt string `json:"boundAt"`
	Note    string `json:"note,omitempty"`
}

// Status is written by the agent to agent_workdir/_status/{machine}.json
type Status struct {
	Machine             string   `json:"machine"`
	BoundUser           string   `json:"boundUser"`
	LocalUsers          []string `json:"localUsers"`
	BoundUserExists     bool     `json:"boundUserExists"`
	LastSync            string   `json:"lastSync"`
	AgentVersion        string   `json:"agentVersion"`
	PolicyETag          string   `json:"policyEtag"`
	CredsETag           string   `json:"credsEtag"`
	BlockEnabled        bool     `json:"blockEnabled"`
	BlockedDomains      int      `json:"blockedDomains"`
	SyncIntervalMinutes int      `json:"syncIntervalMinutes"`
	CredsApplied        bool     `json:"credsApplied"`
	AppLockerMode       string   `json:"appLockerMode"`
	Errors              []string `json:"errors"`
}

// HasLocalUser reports whether a given account has a profile on the machine.
func (s Status) HasLocalUser(name string) bool {
	for _, u := range s.LocalUsers {
		if strings.EqualFold(u, name) {
			return true
		}
	}
	return false
}

// UserEntry is one row of the admin-only roster.
// The account fields are notes for the administrator; nothing logs in with them.
type UserEntry struct {
	WindowsUser   string `json:"windowsUser"`
	CodexAccount  string `json:"codexAccount"`
	ClaudeAccount string `json:"claudeAccount"`
	Enabled       bool   `json:"enabled"`
}

// Users is stored at admin/users.json, outside the agent's directory and
// outside every agent's permissions.
type Users struct {
	Users     []UserEntry `json:"users"`
	UpdatedAt string      `json:"updatedAt"`
}

// Find returns the entry for a Windows user, or nil.
//
// The comparison ignores case because Windows account names do: "Work1" and
// "work1" are the same account, and treating them as two roster entries would
// silently split one employee's credentials across two directories.
func (u Users) Find(windowsUser string) *UserEntry {
	for i := range u.Users {
		if strings.EqualFold(u.Users[i].WindowsUser, windowsUser) {
			return &u.Users[i]
		}
	}
	return nil
}

// CredentialSet is one user's AI tool logins in memory, keyed by the relative
// path inside credentials.zip.
type CredentialSet map[string][]byte

// Paths inside credentials.zip. The agent maps these onto the user profile.
const (
	PathCodexAuth    = "codex/auth.json"
	PathCodexConfig  = "codex/config.toml"
	PathClaudeCreds  = "claude/.credentials.json"
	PathClaudeConfig = "claude.json"
)
