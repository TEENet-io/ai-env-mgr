// Package repo declares what the console stores and how it asks for it.
//
// The interfaces live here, apart from both sides, so that admincore states
// what it needs without knowing about PostgreSQL and dbstore implements it
// without knowing about the business rules. It is also what lets a test use a
// fake for the parts it is not exercising.
//
// Two rules shape every method here:
//
//   - A read that fails says how it failed. ErrNotFound is a fact about the
//     data; anything else is a fact about the database, and the caller must
//     not treat the two alike. Writing defaults over a row that merely could
//     not be read is the failure phase 0 was spent repairing.
//
//   - A write that changes more than one row belongs in InTx. The console's
//     invariants span tables -- re-issuing a credential retires the previous
//     one, supersedes open tasks and enqueues new ones -- and half of that is
//     worse than none of it.
package repo

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	// ErrNotFound means the row is not there. It is an ordinary answer, not a
	// fault: an employee who has never been onboarded has no quota.
	ErrNotFound = errors.New("not found")

	// ErrConflict means somebody else changed the row since it was read. The
	// caller should re-read and decide again rather than retry blindly: the
	// change it was about to make may no longer be the change it wants.
	ErrConflict = errors.New("row changed since it was read")

	// ErrDuplicate means a unique constraint rejected the write -- a Windows
	// user already on the roster, a second open binding for one machine, a
	// second live token for one employee.
	ErrDuplicate = errors.New("already exists")
)

// Store is the whole set of repositories. A Store is either on the pool
// (autocommit, one statement at a time) or inside a transaction; InTx hands
// the second kind to its callback.
type Store interface {
	Employees() Employees
	Quotas() Quotas
	Devices() Devices
	Bindings() Bindings

	// InTx runs fn in a transaction, committing if it returns nil. The Store
	// passed to fn is the transactional one: using the outer Store inside fn
	// would write outside the transaction, which is why fn is given its own.
	InTx(ctx context.Context, fn func(Store) error) error
}

// EmployeeStatus is where a person is in the lifecycle. Offboarded rows stay:
// their history, their audit trail and their machine bindings are the record
// of what happened, and reopening an account reuses the row.
type EmployeeStatus string

const (
	StatusActive     EmployeeStatus = "active"
	StatusOffboarded EmployeeStatus = "offboarded"
)

// Employee is one person on the roster.
//
// ID is the identity. WindowsUser is how the machines and the gateway address
// them today, and it can change; nothing else should be keyed by it.
//
// AuthEpoch goes up whenever the person's credentials must stop working. A
// task created for epoch 4 that comes back from a retry at epoch 5 is
// superseded rather than applied -- which is how a delayed re-provision is
// stopped from handing a token back to somebody who left.
type Employee struct {
	ID            string
	WindowsUser   string
	ExternalID    string // HR number; empty means unknown
	Name          string
	Department    string
	CodexAccount  string // administrator's note: which login this person uses
	ClaudeAccount string
	Status        EmployeeStatus
	AuthEpoch     int
	Version       int
	CreatedAt     time.Time
	UpdatedAt     time.Time
	OffboardedAt  *time.Time
}

// Active reports whether this employee should have working credentials.
func (e Employee) Active() bool { return e.Status == StatusActive }

// NewEmployee is what Create needs. Everything else has a default.
type NewEmployee struct {
	WindowsUser   string
	ExternalID    string
	Name          string
	Department    string
	CodexAccount  string
	ClaudeAccount string
}

// Profile is the set of fields an administrator edits by hand.
type Profile struct {
	Name          string
	Department    string
	CodexAccount  string
	ClaudeAccount string
	ExternalID    string
}

// EmployeeFilter narrows List.
type EmployeeFilter struct {
	// IncludeOffboarded brings back people who have left. The console's main
	// list leaves them out; the audit views want them.
	IncludeOffboarded bool
}

// Employees is the roster.
//
// Every mutation takes the version the caller read, and returns ErrConflict if
// the row has moved on. Two administrators on the same page is not a rare
// case -- it is Tuesday -- and last-write-wins silently discards one of them.
type Employees interface {
	ByID(ctx context.Context, id string) (Employee, error)
	ByWindowsUser(ctx context.Context, windowsUser string) (Employee, error)
	List(ctx context.Context, filter EmployeeFilter) ([]Employee, error)

	// Create adds a person. ErrDuplicate if the Windows user or the external
	// id is taken -- including by somebody who has been offboarded, whose row
	// is still there and should be reopened instead.
	Create(ctx context.Context, e NewEmployee) (Employee, error)

	UpdateProfile(ctx context.Context, id string, version int, p Profile) (Employee, error)

	// Offboard marks the person as gone and raises AuthEpoch, so that anything
	// still in flight for the old epoch is refused. It does not revoke
	// anything by itself: that is a task, committed in the same transaction.
	Offboard(ctx context.Context, id string, version int) (Employee, error)

	// Reopen is the reverse. The epoch is raised again rather than reused: the
	// credentials from before the departure must not come back to life.
	Reopen(ctx context.Context, id string, version int) (Employee, error)

	// BumpAuthEpoch invalidates what is outstanding without changing status --
	// re-issuing a token, or a suspected leak.
	BumpAuthEpoch(ctx context.Context, id string, version int) (Employee, error)

	// SetModels replaces the set of models this employee may call. It is the
	// console's intent; pushing it to the gateway is a task.
	SetModels(ctx context.Context, id string, models []string) error
	Models(ctx context.Context, id string) ([]string, error)
}

// Quota is the limits the console wants the gateway to enforce.
//
// MonthlyBudget is a decimal string ("50", "12.500000"), not a float: it is
// money, it is compared and summed, and binary floating point cannot hold
// 0.1. The gateway holds what has actually been spent; it is deliberately not
// mirrored here, because two copies of one number drift and the truthful one
// is a round trip away.
type Quota struct {
	MonthlyBudget string
	Currency      string
	RPM           int
	TPM           int
	Parallel      int
	// PeriodRule is how the budget rolls over. The gateway resets "1mo"
	// budgets at 00:00 UTC on the 1st regardless of when the account was
	// opened (verified 2026-09-18), so the only value today is
	// calendar_month_utc. PeriodTZ is for display; boundaries are UTC.
	PeriodRule string
	PeriodTZ   string
	Version    int
	UpdatedAt  time.Time
}

const (
	PeriodCalendarMonthUTC = "calendar_month_utc"
	DefaultPeriodTZ        = "Asia/Shanghai"
	DefaultCurrency        = "USD"
)

// Quotas is one current limit set per employee. Previous values are
// recoverable from the audit trail, which is also the only place that records
// who changed them.
type Quotas interface {
	Get(ctx context.Context, employeeID string) (Quota, error)

	// Set writes the limits. expectVersion is the version the caller read, or
	// 0 for "there is none yet" -- so opening an account and editing an
	// existing quota use the same call and neither can clobber the other.
	Set(ctx context.Context, employeeID string, q Quota, expectVersion int) (Quota, error)
}

// NormalizeWindowsUser lower-cases a Windows account name.
//
// Windows treats "Work1" and "work1" as one account and so does the roster;
// storing both would split one person's credentials across two directories.
// The database enforces it too -- this is so the caller gets a clean value
// rather than a constraint violation.
func NormalizeWindowsUser(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// DeviceStatus is how a machine reaches the console. Phase 1 machines are all
// legacy_oss: an agent that reads objects and is recognised by its host name.
// api_v1 is phase 2, where a device has a key of its own. revoked machines are
// refused either way.
type DeviceStatus string

const (
	DeviceLegacyOSS DeviceStatus = "legacy_oss"
	DeviceAPIv1     DeviceStatus = "api_v1"
	DeviceRevoked   DeviceStatus = "revoked"
)

// Device is one machine.
//
// Hostname keeps the spelling the agent reports, because the agent's OSS keys
// are built from it and the console has to reproduce them exactly. Lookups are
// case-insensitive, which is how Windows treats the name.
type Device struct {
	ID           string
	Hostname     string
	Status       DeviceStatus
	AgentVersion string
	Note         string
	LastSeenAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	RevokedAt    *time.Time
}

// DeviceFilter narrows List.
type DeviceFilter struct{ IncludeRevoked bool }

// Devices is the machine inventory.
type Devices interface {
	ByID(ctx context.Context, id string) (Device, error)
	ByHostname(ctx context.Context, hostname string) (Device, error)
	List(ctx context.Context, filter DeviceFilter) ([]Device, error)

	// EnsureByHostname returns the machine, registering it on first sight.
	// Machines arrive by turning up, not by being added in the console, so
	// this is the usual way a row comes to exist.
	EnsureByHostname(ctx context.Context, hostname string) (Device, error)

	// MarkSeen records a sync. agentVersion may be empty, and then whatever is
	// on record is kept: an empty report should not erase a known version.
	MarkSeen(ctx context.Context, id, agentVersion string, at time.Time) error

	// Revoke stops a machine being served. It does not delete it: the
	// bindings and the audit trail are the record of what it had.
	Revoke(ctx context.Context, id string) (Device, error)
}

// Binding is one machine serving one employee, over a span of time.
//
// Epoch counts bindings on that machine. It goes into the exported binding
// object so an agent can tell a re-bind from a re-read of the same thing.
type Binding struct {
	ID         string
	DeviceID   string
	EmployeeID string
	Epoch      int
	Note       string
	BoundAt    time.Time
	BoundBy    string
	UnboundAt  *time.Time
	UnboundBy  string

	// RestartNonce is a one-shot "end this user's Codex" request. The agent
	// echoes the nonce it acted on into its status, so the console can tell a
	// carried-out request from a pending one.
	RestartNonce string
	RestartAt    *time.Time
}

// Open reports whether this binding is the machine's current one.
func (b Binding) Open() bool { return b.UnboundAt == nil }

// Bindings is which machine serves whom.
//
// There is at most one open binding per machine, enforced by a partial unique
// index rather than by remembering to check: two open bindings would mean two
// credential sets racing into one machine.
//
// Rebinding is Unbind then Bind inside InTx. It is deliberately two calls: a
// machine changing hands is exactly when a half-applied change hurts, and a
// caller that has to open a transaction is a caller that has noticed.
type Bindings interface {
	// Open returns the machine's current binding, or ErrNotFound if nobody is
	// assigned to it.
	Open(ctx context.Context, deviceID string) (Binding, error)
	OpenByEmployee(ctx context.Context, employeeID string) ([]Binding, error)
	ListOpen(ctx context.Context) ([]Binding, error)
	History(ctx context.Context, deviceID string) ([]Binding, error)

	// Bind assigns a machine. ErrDuplicate if it already has an open binding.
	Bind(ctx context.Context, deviceID, employeeID, note, by string) (Binding, error)

	// Unbind closes the open binding. ErrNotFound if there is none.
	Unbind(ctx context.Context, deviceID, by string) (Binding, error)

	// RequestCodexRestart puts a fresh nonce on the open binding. A machine
	// nobody is assigned to has nobody whose Codex could be ended, so that is
	// ErrNotFound rather than a silent no-op.
	RequestCodexRestart(ctx context.Context, deviceID, nonce string) (Binding, error)
}
