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
	BlockEnabled   bool     `json:"blockEnabled"`
	BlockedDomains []string `json:"blockedDomains"`
	// AppLockerAllowPaths are program directories every user may execute
	// from, in AppLocker path syntax (`C:\tools\Codex\*`). The image ships
	// AppLocker allowing only Windows and Program Files; anything installed
	// elsewhere -- Codex at C:\tools\Codex -- is blocked for employees the
	// moment AppLocker is enforced. The agent turns this list into rules it
	// owns and never touches the image's own rules. Empty means "manage no
	// rules" (and remove any it previously wrote).
	AppLockerAllowPaths []string `json:"appLockerAllowPaths,omitempty"`
	// AppLockerMode is the enforcement mode the agent holds every configured
	// rule collection in: "enforce", "audit", or empty.
	//
	// Empty is the default and means UNMANAGED: the agent does not touch the
	// machine's enforcement mode at all, which is what every policy object
	// already in the field says and must keep saying. Whether AppLocker
	// blocks is otherwise the image's decision, and an agent must not change
	// a machine's security posture because a field was added.
	//
	// "audit" is the administrator's testing switch: the rules stay exactly
	// where they are and AppLocker keeps logging, it just stops blocking. It
	// is reversible with "enforce"; nothing here ever deletes a rule.
	AppLockerMode       string `json:"appLockerMode,omitempty"`
	SyncIntervalMinutes int    `json:"syncIntervalMinutes"`
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
	// CodexRolloutPct used to stagger the fleet by hashing the machine name.
	// The staging is gone -- publishing now reaches every machine -- but the
	// field remains, and is ALWAYS written, for the agents already in the field.
	//
	// Agents 1.2.5 through 1.2.7 skip the install when this is absent or zero,
	// and the field would be omitted if it were empty. Dropping it would
	// therefore stop Codex updates on every machine still running one of those,
	// silently: no error, no log line, just nothing happening. Newer agents
	// ignore it. Remove it once nothing older than 1.2.8 is left.
	CodexVersion    string `json:"codexVersion,omitempty"`
	CodexSHA256     string `json:"codexSHA256,omitempty"` // hex sha256 of the installer
	CodexKey        string `json:"codexKey,omitempty"`    // object key under _codex/
	CodexRolloutPct int    `json:"codexRolloutPct"`

	UpdatedAt string `json:"updatedAt"`
}

type Application struct {
	AppID          string               `json:"appId"`
	DisplayName    string               `json:"displayName"`
	Publisher      string               `json:"publisher"`
	Version        string               `json:"version"`
	InstallerType  string               `json:"installerType"`
	ObjectKey      string               `json:"objectKey"`
	SHA256         string               `json:"sha256"`
	Size           int64                `json:"size"`
	SilentArgs     []string             `json:"silentArgs,omitempty"`
	Shortcut       ApplicationShortcut  `json:"shortcut,omitempty"`
	Detection      ApplicationDetection `json:"detection"`
	RequiresReboot bool                 `json:"requiresReboot"`
	Enabled        bool                 `json:"enabled"`
	Approved       bool                 `json:"approved"`
	UpdatedAt      string               `json:"updatedAt"`
}
type ApplicationShortcut struct {
	Enabled          bool   `json:"enabled"`
	PublicDesktop    bool   `json:"publicDesktop"`
	Name             string `json:"name"`
	Target           string `json:"target"`
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	Icon             string `json:"icon,omitempty"`
}
type ApplicationDetection struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}
type MachineApplications struct {
	Machine   string               `json:"machine"`
	UpdatedAt string               `json:"updatedAt"`
	Apps      []DesiredApplication `json:"apps"`
}
type DesiredApplication struct {
	AppID          string `json:"appId"`
	Version        string `json:"version"`
	Desired        string `json:"desired"`
	TaskID         string `json:"taskId"`
	AllowDowngrade bool   `json:"allowDowngrade,omitempty"`
}
type ApplicationStatus struct {
	AppID                 string `json:"appId"`
	DesiredVersion        string `json:"desiredVersion"`
	InstalledVersion      string `json:"installedVersion,omitempty"`
	State                 string `json:"state"`
	TaskID                string `json:"taskId,omitempty"`
	PublicDesktopShortcut bool   `json:"publicDesktopShortcut"`
	LaunchAsStandardUser  bool   `json:"launchAsStandardUser"`
	AppLockerAllowed      bool   `json:"appLockerAllowed"`
	RebootRequired        bool   `json:"rebootRequired"`
	LastError             string `json:"lastError,omitempty"`
	UpdatedAt             string `json:"updatedAt"`
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

	// RestartCodex is a one-shot request from the console: a fresh nonce asks
	// the agent to end the bound user's Codex once. The agent remembers the
	// last nonce it acted on, so re-reading the same binding every cycle --
	// which is what the agent does -- costs nobody their work a second time.
	//
	// It rides inside the binding rather than in an object of its own because
	// the binding is already the one thing every agent reads every cycle, and
	// the agent's OSS role can read _bindings/ and almost nothing else. A new
	// prefix would mean a new grant on the role for every machine in the
	// fleet, to carry sixteen bytes.
	RestartCodex string `json:"restartCodex,omitempty"` // nonce
	// RestartCodexAt is when the console asked, for display only.
	RestartCodexAt string `json:"restartCodexAt,omitempty"` // RFC3339

	// AgentTarget and CodexTarget are this machine's release targets (agent
	// 1.2.16+). Present, they REPLACE the fleet-wide target in policy.json
	// for this machine -- a present target with an empty Version means "this
	// machine: nothing". Absent, the fleet policy applies as before, which is
	// what every binding object already in the bucket says. Older agents
	// ignore the fields.
	AgentTarget *ReleaseTarget `json:"agentTarget,omitempty"`
	CodexTarget *ReleaseTarget `json:"codexTarget,omitempty"`

	// SyncRequested is the console's "sync now" nonce. The agent does not
	// act on the value: any change to this object is what its heartbeat
	// notices, and this field exists so that a request changes the object.
	SyncRequested string `json:"syncRequested,omitempty"`
}

// ReleaseTarget is one product's target for one machine: the version, the
// checksum the download must match, where the package is, and a generation
// that the machine echoes back so the console can tell which decision a
// report is about. A new generation of the same version is a new attempt.
type ReleaseTarget struct {
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	Key        string `json:"key,omitempty"` // object key; empty = the product's default key
	Generation int    `json:"generation"`
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

	// The targets this machine acted on in this cycle, with their
	// generation, so the console can tell which decision a report answers.
	// CodexDeferReason says why an eligible install waited: "in_use" or
	// "disk". AgentUpdateState is "pending" when a verified binary is about
	// to be applied after this report, and "failed" when an earlier cycle
	// tried this generation and this is still the old binary.
	CodexTarget           string `json:"codexTarget,omitempty"`
	CodexTargetGeneration int    `json:"codexTargetGeneration,omitempty"`
	CodexDeferReason      string `json:"codexDeferReason,omitempty"`
	AgentUpdateTarget     string `json:"agentUpdateTarget,omitempty"`
	AgentUpdateGeneration int    `json:"agentUpdateGeneration,omitempty"`
	AgentUpdateState      string `json:"agentUpdateState,omitempty"`

	// CodexRestartNonce is the last Binding.RestartCodex this machine acted
	// on. The console compares it with the nonce it wrote: equal means the
	// request has been carried out, different means it is still pending.
	// Without it a one-shot request would have no visible outcome at all --
	// the administrator would press the button and learn nothing.
	CodexRestartNonce string `json:"codexRestartNonce,omitempty"`
	// CodexRestartAt is when the agent acted (RFC3339), and CodexRestartNote
	// is what came of it: "killed 1 process", "no process", or the error.
	CodexRestartAt   string              `json:"codexRestartAt,omitempty"`
	CodexRestartNote string              `json:"codexRestartNote,omitempty"`
	Apps             []ApplicationStatus `json:"apps,omitempty"`
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
	WindowsUser  string `json:"windowsUser"`
	CodexAccount string `json:"codexAccount"`
	Email        string `json:"email,omitempty"`
	// Name and Department are labels for the administrator's benefit and
	// are mirrored onto the gateway user (user_alias, metadata.department)
	// so the gateway UI shows the same person. The roster is authoritative.
	Name       string `json:"name,omitempty"`
	Department string `json:"department,omitempty"`
	Enabled    bool   `json:"enabled"`
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
//
// Claude Code is no longer managed (2026-09-18): its two entries are gone
// from here, and an archive that still carries them is delivered without
// them. LegacyClaudeEntries names them for the cleanup that removes what an
// earlier agent put on a machine.
const (
	PathCodexAuth   = "codex/auth.json"
	PathCodexConfig = "codex/config.toml"
	PathCodexModels = "codex/models.json"
)

// LegacyClaudeEntries are the archive paths an earlier console published for
// Claude Code. They are recognised only to be dropped.
var LegacyClaudeEntries = []string{"claude/.credentials.json", "claude.json"}
