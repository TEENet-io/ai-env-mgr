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
	Policies() Policies
	Settings() Settings
	Tasks() Tasks
	Audit() Audit
	Grants() Grants
	Credentials() Credentials

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

// PolicyVersion is one published policy. Published policies are immutable:
// editing a live one is how a bad AppLocker path reaches every machine with
// nothing to roll back to. A rollback here is pointing Current at an older
// version.
//
// Content is the policy JSON exactly as an agent will receive it. It is not
// parsed on the way in or out: validation belongs with the rules (model
// .ValidateAppLockerPath, interval clamping), and a second implementation in
// SQL would be a second answer to the same question.
type PolicyVersion struct {
	Version   int64
	Content   []byte
	Note      string
	CreatedBy string
	CreatedAt time.Time
}

// Policies holds every published policy and which one the fleet should be on.
type Policies interface {
	// Current is the version the fleet should be running. ErrNotFound before
	// anything has ever been published, which the caller answers with the
	// built-in default -- but only for ErrNotFound, never for a read that
	// failed.
	Current(ctx context.Context) (PolicyVersion, error)

	// Publish stores content as a new version and makes it current.
	Publish(ctx context.Context, content []byte, note, by string) (PolicyVersion, error)

	// Rollback makes an existing version current again without copying it, so
	// the history reads as what happened rather than as a new decision.
	Rollback(ctx context.Context, version int64, by string) (PolicyVersion, error)

	ByVersion(ctx context.Context, version int64) (PolicyVersion, error)
	List(ctx context.Context, limit int) ([]PolicyVersion, error)
}

// Setting is one global switch or default: the quota a new account is
// pre-filled with, an alert channel, a feature flag. Key/value because each
// has its own shape and they are read one at a time by name.
type Setting struct {
	Key       string
	Value     []byte
	Version   int
	UpdatedAt time.Time
	UpdatedBy string
}

// Settings keys in use. They are constants so a typo is a compile error rather
// than a silently missing setting that falls back to a default.
const (
	SettingQuotaDefaults = "quota_defaults"
)

// Settings is the global configuration that is not policy.
type Settings interface {
	// Get returns the stored value. ErrNotFound means nothing has been saved
	// yet; anything else means the database could not be read, and the caller
	// must not fall back to a default on it.
	Get(ctx context.Context, key string) (Setting, error)

	// Set writes it. expectVersion is the version the caller read, or 0 for
	// "there is none yet", so a first save and an edit use the same call and
	// neither can quietly overwrite the other.
	Set(ctx context.Context, key string, value []byte, expectVersion int, by string) (Setting, error)

	List(ctx context.Context) ([]Setting, error)
}

// ErrLeaseLost means the worker no longer holds the task it is reporting on:
// its lease expired, or somebody else has taken it. The result is not recorded
// and the task is left for whoever holds it now. Reconciliation, not a retry,
// is the answer -- the work may well have been done.
var ErrLeaseLost = errors.New("task lease is no longer held")

// TaskStatus is where a task is.
//
// retry_wait and pending are both runnable; the difference is only whether it
// has failed before, which is worth seeing in a list. superseded is for work
// that was overtaken -- a provision for an epoch the employee has moved past.
type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskRunning    TaskStatus = "running"
	TaskRetryWait  TaskStatus = "retry_wait"
	TaskSucceeded  TaskStatus = "succeeded"
	TaskFailed     TaskStatus = "failed"
	TaskSuperseded TaskStatus = "superseded"
)

// Task kinds. Everything the console does outside its own database is one of
// these, committed with the change that asked for it.
const (
	TaskGatewayProvision = "gateway_provision"
	TaskGatewayRevoke    = "gateway_revoke"
	TaskOSSExport        = "oss_export"
	TaskAuditPublish     = "audit_publish"
	TaskReconcile        = "reconcile"
)

// Task is one unit of work for the Worker.
//
// Payload holds references, never secrets: a credential row id, not a token.
// Tasks are listed in the console, dumped during an incident and pasted into
// tickets.
type Task struct {
	ID             string
	Kind           string
	IdempotencyKey string
	Payload        []byte
	Status         TaskStatus
	Attempts       int
	MaxAttempts    int
	LeaseUntil     *time.Time
	LeaseOwner     string
	NextRunAt      time.Time
	LastError      string
	// TargetEpoch is the employee epoch this task was created for. A task that
	// comes back from a retry after the employee has moved on is superseded
	// rather than applied.
	TargetEpoch *int
	EmployeeID  string
	DeviceID    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	FinishedAt  *time.Time
}

// Open reports whether this task still has work to do.
func (t Task) Open() bool {
	return t.Status == TaskPending || t.Status == TaskRunning || t.Status == TaskRetryWait
}

// NewTask is what Enqueue needs.
//
// IdempotencyKey is what makes at-least-once delivery safe. Derive it from the
// change, not from the moment -- employee id, epoch and kind -- so that a retry
// after an ambiguous failure finds the existing task instead of provisioning a
// second time.
type NewTask struct {
	Kind           string
	IdempotencyKey string
	Payload        []byte
	TargetEpoch    *int
	EmployeeID     string
	DeviceID       string
	MaxAttempts    int
	// NotBefore delays the first attempt. Zero means now.
	NotBefore time.Time
}

// TaskAttempt is one execution of a task, kept whether it worked or not:
// "it succeeded on the fourth try, ninety minutes late" is the interesting
// case and a table of failures alone cannot show it.
type TaskAttempt struct {
	ID          int64
	TaskID      string
	Attempt     int
	Owner       string
	StartedAt   time.Time
	EndedAt     *time.Time
	Outcome     string
	ErrorClass  string
	ErrorDetail string
	// ExternalRef is the upstream's own request id when it gives one. It is
	// what turns "the gateway rejected it" into something the gateway's
	// operator can look up.
	ExternalRef string
}

// Tasks is the persistent queue.
type Tasks interface {
	// Enqueue adds a task, or returns the one that is already there for this
	// idempotency key. created says which happened, so a caller can tell a
	// fresh request from a repeat without comparing timestamps.
	Enqueue(ctx context.Context, t NewTask) (task Task, created bool, err error)

	// Claim takes one runnable task and leases it. kinds narrows what this
	// worker will take; empty means anything. ErrNotFound when there is
	// nothing to do, which is the ordinary case and not a fault.
	//
	// Two workers claiming at once get different tasks, or one gets nothing.
	Claim(ctx context.Context, owner string, kinds []string, lease time.Duration) (Task, error)

	// Extend pushes the lease out for work that is taking a while. Without it
	// a slow task is picked up a second time while it is still running.
	Extend(ctx context.Context, id, owner string, lease time.Duration) error

	// Succeed closes the task. externalRef is the upstream's request id, if
	// there is one. ErrLeaseLost if this worker no longer holds it.
	Succeed(ctx context.Context, id, owner, externalRef string) error

	// Fail records a failed attempt. The task goes back to retry_wait until
	// its attempts run out, and then to failed -- retrying for ever turns one
	// broken task into a permanent load on whatever it is calling.
	Fail(ctx context.Context, id, owner string, retryAt time.Time, errorClass, detail, externalRef string) (Task, error)

	// Supersede abandons a task that has been overtaken by events.
	Supersede(ctx context.Context, id, reason string) error

	// SupersedeOpenForEmployee abandons every open task for an employee that
	// targets an epoch older than belowEpoch. This is what offboarding and
	// re-issuing call, in the same transaction as the epoch bump.
	SupersedeOpenForEmployee(ctx context.Context, employeeID string, belowEpoch int) (int, error)

	ByID(ctx context.Context, id string) (Task, error)
	ListOpen(ctx context.Context, limit int) ([]Task, error)
	Attempts(ctx context.Context, taskID string) ([]TaskAttempt, error)

	// ReleaseExpiredLeases puts tasks whose worker died back on the queue. A
	// lease is not a lock: a worker that stops holds nothing.
	ReleaseExpiredLeases(ctx context.Context) (int, error)
}

// Actor types for audit events.
const (
	ActorAdmin  = "admin"
	ActorDevice = "device"
	ActorWorker = "worker"
	ActorSystem = "system"
)

// AuditEvent is one recorded action.
//
// Before and After are the redacted values the console shows. Secrets are
// replaced upstream, in eventlog.Redact, before they ever reach a column: a
// table that the application account can only append to is still a table
// people read.
//
// EventID is generated on insert and is the same id as the SLS audit copy, so
// the two can be reconciled line by line.
type AuditEvent struct {
	EventID    string
	OccurredAt time.Time
	ActorType  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Before     []byte
	After      []byte
	Detail     []byte
	Result     string
	TaskID     string
	RequestID  string
}

// Audit is the append-only history.
//
// There is no Update and no Delete, and the application database account does
// not hold those rights either. A convention in Go would not survive a bug; a
// grant survives both a bug and a stolen password.
type Audit interface {
	// Append records an event and returns its id. Call it inside the same
	// transaction as the change it describes, so an action that happened
	// cannot end up unrecorded, and one that was rolled back cannot end up
	// recorded.
	Append(ctx context.Context, ev AuditEvent) (string, error)

	ByTarget(ctx context.Context, targetType, targetID string, limit int) ([]AuditEvent, error)
	Recent(ctx context.Context, limit int) ([]AuditEvent, error)
	ByID(ctx context.Context, eventID string) (AuditEvent, error)

	// PendingDelivery lists events not yet confirmed in target (SLS, today).
	// Delivery is at-least-once, so the far side may hold duplicates and
	// queries there deduplicate on event id.
	PendingDelivery(ctx context.Context, target string, limit int) ([]AuditEvent, error)
	ConfirmDelivery(ctx context.Context, eventID, target string) error
	RecordDeliveryFailure(ctx context.Context, eventID, target, reason string) error
}

// Grant states. desired is what the console wants; actual is what it last
// observed on the gateway. They are separate because a provisioning call that
// times out leaves us genuinely unsure, and a single state column would have to
// lie in one direction or the other.
const (
	GrantActive  = "active"
	GrantRevoked = "revoked"

	ActualUnknown = "unknown"
	ActualActive  = "active"
	ActualRevoked = "revoked"
	// ActualMissing is the gateway saying the key is not there: success for a
	// revoke, a fault for a grant that is supposed to be live.
	ActualMissing = "missing"
)

// Grant is one employee's authorisation on one gateway.
type Grant struct {
	ID           string
	EmployeeID   string
	Epoch        int
	Gateway      string
	ExternalUser string // emp-<windows user>, kept across a rename
	KeyAlias     string
	Models       []string
	Desired      string
	Actual       string
	// CredentialID points at the encrypted token this grant issued. The token
	// itself is never on this row.
	CredentialID string
	ReconciledAt *time.Time
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewGrant is what Create needs.
type NewGrant struct {
	EmployeeID   string
	Epoch        int
	Gateway      string
	ExternalUser string
	KeyAlias     string
	Models       []string
	CredentialID string
}

// Grants is what the console has asked each gateway for.
type Grants interface {
	// Active returns the employee's live grant on a gateway, ErrNotFound if
	// there is none. At most one can exist: two live tokens for one person
	// means revoking "the" token leaves the other one working.
	Active(ctx context.Context, gateway, employeeID string) (Grant, error)
	ByEmployee(ctx context.Context, employeeID string) ([]Grant, error)
	ByKeyAlias(ctx context.Context, gateway, keyAlias string) (Grant, error)

	Create(ctx context.Context, g NewGrant) (Grant, error)

	// Revoke records the intent. The gateway call is a task; what the gateway
	// actually did comes back through RecordActual.
	Revoke(ctx context.Context, id string) (Grant, error)

	// RecordActual stores what the gateway was observed to hold. reason is
	// kept for a grant that could not be checked.
	RecordActual(ctx context.Context, id, actual, reason string) (Grant, error)

	// NeedsReconcile lists grants whose observed state is unknown or does not
	// match the intent, oldest check first.
	NeedsReconcile(ctx context.Context, gateway string, staleAfter time.Duration, limit int) ([]Grant, error)
}

// Credential is one stored secret, encrypted.
//
// ContentSHA256 is the hash of the delivered bytes, so "has this machine
// already got this version" can be answered without decrypting anything.
type Credential struct {
	ID            string
	EmployeeID    string
	Epoch         int
	Purpose       string
	Ciphertext    []byte
	KeyVersion    string
	ContentSHA256 []byte
	CreatedAt     time.Time
	RetiredAt     *time.Time
}

// Credential purposes.
const PurposeCodexGateway = "codex_gateway"

// NewCredential is what Store needs. The plaintext never appears: the caller
// seals it first (internal/secrets) and passes the blob.
type NewCredential struct {
	EmployeeID    string
	Epoch         int
	Purpose       string
	Ciphertext    []byte
	KeyVersion    string
	ContentSHA256 []byte
}

// Credentials holds the encrypted values.
type Credentials interface {
	// Live returns the current credential for an employee and purpose.
	Live(ctx context.Context, employeeID, purpose string) (Credential, error)
	ByID(ctx context.Context, id string) (Credential, error)

	// Store saves a new credential and retires the previous live one in the
	// same statement. Two live credentials for one purpose would mean the
	// exporter could deliver either.
	Store(ctx context.Context, c NewCredential) (Credential, error)

	// Retire ends the live credential without issuing a replacement, which is
	// what offboarding does. It is not an error if there is none.
	Retire(ctx context.Context, employeeID, purpose string) (int, error)
}
