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
	// Collection of raw AI session files. Off by default: the code ships
	// before the feature is enabled, and the agent's write-only permission on
	// data_collect is added only when an administrator turns this on. See
	// OSS布局.md §6.
	CollectEnabled      bool   `json:"collectEnabled"`
	CollectQuietSeconds int    `json:"collectQuietSeconds,omitempty"` // debounce; 0 -> default 60
	CollectSince        string `json:"collectSince,omitempty"`        // YYYY-MM-DD UTC; empty -> all history

	// Agent self-update. Empty version means "do not update". When set, an
	// agent whose own version differs downloads the binary at
	// ossclient.AgentBinaryKey(), checks its SHA-256 against AgentUpdateSHA256,
	// and only then replaces itself and restarts. Clearing the version is the
	// remote kill switch. See OSS布局.md §7.
	AgentUpdateVersion string `json:"agentUpdateVersion,omitempty"`
	AgentUpdateSHA256  string `json:"agentUpdateSHA256,omitempty"` // hex sha256 of the target binary

	// Codex desktop distribution. Empty version means "do not distribute" and
	// is the remote kill switch, exactly like AgentUpdateVersion above.
	//
	// The agent compares for EQUALITY, not for "newer": it installs whenever
	// the version recorded on the machine differs from CodexVersion. Setting
	// the target back to an older version is therefore a rollback, which an
	// upgrade-only rule could not express -- and a bad build is precisely when
	// you need to go backwards.
	//
	// CodexRolloutPct staggers the fleet: a machine updates only when
	// crc32(machine) % 100 < CodexRolloutPct, so the same machines are always
	// in the first ring. 0 reaches nobody, 100 reaches everyone. Since the
	// package is a repackaging of an upstream build whose patches drift, start
	// small. See docs/Codex分发方案.md.
	CodexVersion    string `json:"codexVersion,omitempty"`
	CodexSHA256     string `json:"codexSHA256,omitempty"` // hex sha256 of the installer
	CodexKey        string `json:"codexKey,omitempty"`    // object key under _codex/
	CodexRolloutPct int    `json:"codexRolloutPct,omitempty"`

	UpdatedAt string `json:"updatedAt"`
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
	CollectEnabled      bool     `json:"collectEnabled"`
	CollectUploaded     int      `json:"collectUploaded"`

	// Errors are things that failed: a policy that would not apply, a download
	// that broke, credentials that could not be delivered. Something has to be
	// done about each one.
	//
	// Warnings are states worth saying out loud that are not faults -- a
	// machine nobody has assigned yet, an employee nobody has signed in for,
	// credentials removed because somebody was offboarded. Mixing them in with
	// Errors made ordinary onboarding look broken, and a console that shows
	// red for normal states is one people stop reading.
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings,omitempty"`

	// LastEvent records the agent's most recent lifecycle transition that it
	// managed to report BEFORE going quiet: "suspend" (the machine is about to
	// sleep) or "stopped" (the service was stopped or the machine is shutting
	// down). It is empty during normal operation and, crucially, stays empty
	// when an agent crashes -- a crash sends no warning, so "quiet with no
	// event" is what lets the admin tell a dead agent apart from one that only
	// went to sleep. A normal sync or heartbeat clears it again. LastEventAt is
	// when the event was reported (RFC3339).
	LastEvent   string `json:"lastEvent,omitempty"`
	LastEventAt string `json:"lastEventAt,omitempty"`

	// CodexVersion is what this machine actually has installed, which is how
	// an administrator tells a published version from a delivered one.
	// CodexState explains a machine that is eligible but has not taken it:
	// "deferred" (Codex was in use, or the disk was too full) or "failed".
	CodexVersion string `json:"codexVersion,omitempty"`
	CodexState   string `json:"codexState,omitempty"`
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
