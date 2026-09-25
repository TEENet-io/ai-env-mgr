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
	Admins() Admins
	LegacyIDs() LegacyIDs
	Reports() Reports
	Releases() Releases
	Usage() Usage
	Alerts() Alerts
	DeviceTokens() DeviceTokens
	CredentialBundles() CredentialBundles
	ApplicationTasks() ApplicationTasks

	// InTx runs fn in a transaction, committing if it returns nil. The Store
	// passed to fn is the transactional one: using the outer Store inside fn
	// would write outside the transaction, which is why fn is given its own.
	InTx(ctx context.Context, fn func(Store) error) error

	// Lock takes an exclusive lock on a name for the rest of the current
	// transaction, waiting for whoever holds it. It is only valid inside InTx.
	//
	// It serialises work that reads the database and then writes somewhere
	// the database cannot see -- an OSS object, say. Without it two workers
	// exporting the same employee, or one whose lease expired and one that
	// took over, can each read and then write in the wrong order, and the
	// older bytes land last.
	Lock(ctx context.Context, name string) error
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
	ID           string
	WindowsUser  string
	ExternalID   string // HR number; empty means unknown
	Name         string
	Department   string
	CodexAccount string // administrator's note: which login this person uses
	Email        string // where the cloud desktop's verification codes go (a note; nothing sends to it)
	Status       EmployeeStatus
	AuthEpoch    int
	Version      int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	OffboardedAt *time.Time
	// DeletedAt is set when an offboarded account is removed from the
	// console: the row stays for history, the name is free for reuse.
	DeletedAt *time.Time
}

// Active reports whether this employee should have working credentials.
func (e Employee) Active() bool { return e.Status == StatusActive }

// Deleted reports whether the account has been removed from the console.
func (e Employee) Deleted() bool { return e.DeletedAt != nil }

// NewEmployee is what Create needs. Everything else has a default.
type NewEmployee struct {
	WindowsUser  string
	ExternalID   string
	Name         string
	Department   string
	CodexAccount string
	Email        string
}

// Profile is the set of fields an administrator edits by hand.
type Profile struct {
	Name         string
	Department   string
	CodexAccount string
	Email        string
	ExternalID   string
}

// EmployeeFilter narrows List.
type EmployeeFilter struct {
	// IncludeOffboarded brings back people who have left. The console's main
	// list leaves them out; the audit views want them.
	IncludeOffboarded bool
	// IncludeDeleted brings back accounts that were deleted. Only the list
	// page's "deleted" filter wants them; nothing else should see them.
	IncludeDeleted bool
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

	// Delete removes an offboarded account from the console. The row is
	// kept; List and ByWindowsUser stop returning it, and its Windows user
	// name may be used again. Deleting an active account is refused, and
	// deleting twice is not an error.
	Delete(ctx context.Context, id string, version int) (Employee, error)

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
	// SyncNonce is the last "sync now" request; it rides in the binding
	// object so that the object changes and the agent notices.
	SyncNonce string

	// Channel is how the agent talks to us: "oss" reads objects from the
	// bucket, "api" holds a device token and asks the console.
	Channel             string
	EnrolledAt          *time.Time
	EnrolledFrom        string // the address the enrolment came from
	ReenrolAllowedUntil *time.Time
	// LogTail is the last log excerpt the agent sent, replacing the bucket's
	// _logs/ object for machines on the api channel.
	LogTail   string
	LogTailAt *time.Time
}

const (
	ChannelOSS = "oss"
	ChannelAPI = "api"
)

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

	// Reactivate brings a forgotten machine back: it enrolled again, so
	// somebody switched it on. The row keeps its history.
	Reactivate(ctx context.Context, id string) (Device, error)
	// SetEnrolled records that the machine now talks to the console
	// directly, and where the enrolment came from.
	SetEnrolled(ctx context.Context, id, from string, at time.Time) error
	// AllowReenrol opens a window in which a machine whose token is still
	// live may enrol again -- a reinstalled machine, a lost token.
	AllowReenrol(ctx context.Context, id string, until time.Time) error
	// SetLogTail stores the agent's latest log excerpt.
	SetLogTail(ctx context.Context, id, tail string, at time.Time) error

	// Revoke stops a machine being served. It does not delete it: the
	// bindings and the audit trail are the record of what it had.
	Revoke(ctx context.Context, id string) (Device, error)

	// RequestSync records a "sync now" nonce for the machine.
	RequestSync(ctx context.Context, id, nonce string) (Device, error)
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
	// HistoryByEmployee is every machine this person has held, newest first.
	HistoryByEmployee(ctx context.Context, employeeID string) ([]Binding, error)

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
	TaskGatewayDelete    = "gateway_delete"
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
	// Enqueue adds a task, or returns the one already queued under the same
	// idempotency key. A task that had failed for good is put back on the
	// queue and reported as created: asking again is the retry.
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

	// FailPermanently closes a task that will never succeed however often it
	// is tried: an unknown kind, a malformed payload, an upstream that says
	// the request itself is wrong. Retrying those is load without hope, and it
	// buries the real failures in a list that never empties.
	FailPermanently(ctx context.Context, id, owner, errorClass, detail, externalRef string) (Task, error)

	// Supersede abandons a task that has been overtaken by events.
	Supersede(ctx context.Context, id, reason string) error

	// SupersedeOpenForEmployee abandons every open task for an employee that
	// targets an epoch older than belowEpoch. This is what offboarding and
	// re-issuing call, in the same transaction as the epoch bump.
	SupersedeOpenForEmployee(ctx context.Context, employeeID string, belowEpoch int) (int, error)

	ByID(ctx context.Context, id string) (Task, error)
	ListOpen(ctx context.Context, limit int) ([]Task, error)
	// ListRecent is the newest tasks in any state, for the console's task
	// page: what is queued, what failed, what just ran.
	ListRecent(ctx context.Context, limit int) ([]Task, error)
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

// AuditFilter narrows Search. Empty strings and zero times mean "any".
type AuditFilter struct {
	TargetType, TargetID, Action, ActorID string
	From, To                              time.Time
	Offset, Limit                         int
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
	// Search lists events newest first that match the filter, and how many
	// match in all, for paging.
	Search(ctx context.Context, f AuditFilter) (events []AuditEvent, total int, err error)
	ByID(ctx context.Context, eventID string) (AuditEvent, error)

	// CountByRequestPrefix counts events whose request id starts with prefix.
	// The importer uses it to know how much of an append-only history file
	// it has already brought in, so re-running does not duplicate the rest.
	CountByRequestPrefix(ctx context.Context, prefix string) (int, error)

	// PendingDelivery lists events not yet confirmed in target (SLS, today).
	// Delivery is at-least-once, so the far side may hold duplicates and
	// queries there deduplicate on event id.
	PendingDelivery(ctx context.Context, target string, limit int) ([]AuditEvent, error)

	// MarkDeliveryWritten records that the event has been handed to whatever
	// ships it -- written to the file the collector tails, in today's setup.
	// That is not the same as having arrived, which is why it is a separate
	// column and a separate call: a collector that dies with a full disk hands
	// back no error to anybody.
	MarkDeliveryWritten(ctx context.Context, eventID, target string) error

	// AwaitingConfirmation lists events written before the given moment that
	// have still not been seen at the far end. Old ones that never arrive are
	// the whole point of keeping the two apart.
	AwaitingConfirmation(ctx context.Context, target string, writtenBefore time.Time, limit int) ([]AuditEvent, error)

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

	// SetModels records the allowlist the token on the gateway now carries.
	SetModels(ctx context.Context, id string, models []string) (Grant, error)

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

	// LiveOlderThan lists live credentials for a purpose created before the
	// given time, oldest first, at most limit: the rotation's worklist.
	LiveOlderThan(ctx context.Context, purpose string, before time.Time, limit int) ([]Credential, error)

	// NotSealedWith lists live credentials wrapped by any master key but
	// this one, at most limit: the re-keying's worklist. Retired rows are
	// left as they are; the old key stays until nothing references it.
	NotSealedWith(ctx context.Context, keyVersion string, limit int) ([]Credential, error)
	// Reseal replaces the ciphertext and key version of one row.
	Reseal(ctx context.Context, id string, ciphertext []byte, keyVersion string) error
	// KeyVersions lists every master key version any row, live or retired,
	// still refers to.
	KeyVersions(ctx context.Context) ([]string, error)
}

// Admin roles, least to most. Phase 1 uses admin for everybody; the others
// exist so that narrowing later is a change of one column, not a migration.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleSecurity = "security"
	RoleAdmin    = "admin"
)

// Admin is somebody who can sign in to the console.
//
// This replaces "paste an OSS AccessKey into the login form". A session must
// not carry cloud credentials, and taking one person's access away must not
// mean rotating a key everybody shares.
//
// TOTPSecret is the sealed seed, never the seed. Nil means enrolment is not
// finished: such an account may sign in only to complete it.
type Admin struct {
	ID             string
	Username       string
	Email          string
	PasswordHash   string
	PasswordSetAt  time.Time
	TOTPSecret     []byte
	TOTPKeyVersion string
	TOTPEnrolledAt *time.Time
	RecoveryHashes []string
	Role           string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastLoginAt    *time.Time
	DisabledAt     *time.Time
}

// Enabled reports whether this account may sign in at all.
func (a Admin) Enabled() bool { return a.DisabledAt == nil }

// TOTPEnrolled reports whether a second factor is set up.
func (a Admin) TOTPEnrolled() bool { return len(a.TOTPSecret) > 0 && a.TOTPEnrolledAt != nil }

// NewAdmin is what Create needs. The password arrives hashed: this layer never
// sees a plaintext password, so it cannot log one.
type NewAdmin struct {
	Username     string
	Email        string
	PasswordHash string
	Role         string
}

// Session is a signed-in console session.
//
// TokenSHA256 is the hash of the cookie value, never the value: a database
// dump, a backup or a slow query log must not hand anybody a working session.
//
// The two expiry times are separate on purpose. Idle timeout alone would let
// one sign-in last for ever as long as somebody keeps a tab open.
type Session struct {
	TokenSHA256       []byte
	PrincipalID       string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	AbsoluteExpiresAt time.Time
	LastSeenAt        time.Time
	CreatedIP         string
	UserAgent         string
}

// Login outcomes, recorded for rate limiting and for noticing a password
// spray. They are deliberately specific: "bad password" and "unknown user"
// look the same to the person signing in, and must not look the same to
// whoever reads the table afterwards.
const (
	LoginOK          = "ok"
	LoginBadPassword = "bad_password"
	LoginBadTOTP     = "bad_totp"
	LoginDisabled    = "disabled"
	LoginUnknownUser = "unknown_user"
	LoginLocked      = "locked"
)

// LoginAttempt is one sign-in try.
type LoginAttempt struct {
	Username string
	SourceIP string
	At       time.Time
	Outcome  string
}

// Admins is the console's own account store.
type Admins interface {
	ByID(ctx context.Context, id string) (Admin, error)
	ByUsername(ctx context.Context, username string) (Admin, error)
	List(ctx context.Context) ([]Admin, error)

	// CountEnabled is what the first-run check asks. Zero means the console
	// has no way in yet and should offer to create one.
	CountEnabled(ctx context.Context) (int, error)

	Create(ctx context.Context, a NewAdmin) (Admin, error)
	SetPasswordHash(ctx context.Context, id, hash string) error

	// SetTOTP stores the sealed seed and the hashed recovery codes together:
	// enrolling without recovery codes is how somebody locks themselves out
	// with a lost phone.
	SetTOTP(ctx context.Context, id string, sealedSecret []byte, keyVersion string, recoveryHashes []string) error

	// ReplaceRecoveryHashes is used when a code is consumed. The whole list is
	// written back, so a consumed code cannot be used twice.
	ReplaceRecoveryHashes(ctx context.Context, id string, hashes []string) error
	// ResealTOTP replaces the sealed seed and its key version, nothing else.
	ResealTOTP(ctx context.Context, id string, sealedSecret []byte, keyVersion string) error

	RecordLogin(ctx context.Context, id string, at time.Time) error
	SetDisabled(ctx context.Context, id string, disabled bool) (Admin, error)

	// RecordAttempt writes one sign-in attempt, successful or not.
	RecordAttempt(ctx context.Context, a LoginAttempt) error

	// RecentFailures counts failed attempts in a window, for the lockout. Both
	// the account and the source address are counted: one protects a person's
	// password, the other notices somebody trying many accounts.
	RecentFailures(ctx context.Context, username, sourceIP string, window time.Duration) (byUser, byIP int, err error)

	CreateSession(ctx context.Context, s Session) error
	// SessionByToken returns the session and its owner, or ErrNotFound if it
	// does not exist, has expired, or belongs to a disabled account.
	SessionByToken(ctx context.Context, tokenSHA256 []byte) (Session, Admin, error)
	TouchSession(ctx context.Context, tokenSHA256 []byte, expiresAt time.Time) error
	DeleteSession(ctx context.Context, tokenSHA256 []byte) error
	// DeleteSessionsFor ends every session of one account, which is what a
	// password change and a disable both have to do.
	DeleteSessionsFor(ctx context.Context, principalID string) (int, error)
	DeleteExpiredSessions(ctx context.Context) (int, error)
}

// LegacyIDs is the bridge to the world of names.
//
// Before the database, an employee was a Windows user name and a machine was a
// host name. Those names appear in old audit lines, in OSS keys and in the
// gateway's own records, and long after the migration somebody will need to
// know which row "work1" was. The map stays; it costs a row each.
type LegacyIDs interface {
	Record(ctx context.Context, kind, legacyKey, newID string) error
	Lookup(ctx context.Context, kind, legacyKey string) (string, error)
}

// Kinds of legacy key.
const (
	LegacyEmployee = "employee"
	LegacyDevice   = "device"
)

// DeviceReport is what a machine last said about itself: the newest
// _status/<machine> object, imported by the Worker.
//
// It is a cache of the agent's report and never a source of truth about what
// the machine is supposed to have. The typed columns are the ones the console
// lists and filters on; Report keeps the whole object so a field added to the
// agent is visible before any migration.
type DeviceReport struct {
	DeviceID          string
	ImportedAt        time.Time
	LastSyncAt        *time.Time
	AgentVersion      string
	BoundWindowsUser  string
	BoundUserExists   *bool
	PolicyETag        string
	CredsETag         string
	CredsApplied      *bool
	AppLockerMode     string
	CollectEnabled    *bool
	CollectUploaded   int
	CodexVersion      string
	CodexState        string
	CodexRestartNonce string
	CodexRestartAt    *time.Time
	CodexRestartNote  string
	LastEvent         string
	LastEventAt       *time.Time
	ErrorCount        int
	WarningCount      int
	Report            []byte
	SourceETag        string
}

// Reports is the machines' own account of themselves.
type Reports interface {
	Get(ctx context.Context, deviceID string) (DeviceReport, error)
	List(ctx context.Context) ([]DeviceReport, error)

	// Import stores a report unless it is unchanged (same source etag) or
	// older than the one already held. Object stores do not promise read
	// ordering, and a stale status overwriting a fresh one reads as a machine
	// going dark. It reports whether anything was written.
	Import(ctx context.Context, r DeviceReport) (bool, error)
}

// UsageRow is one day of one token's calls to one model group, as the
// gateway logs recorded them. CostUSD is the numeric column as text, the
// same way Quota carries money: nothing here does arithmetic in floats.
type UsageRow struct {
	Day              time.Time
	EmployeeID       string // "" when the alias matched nobody
	KeyAlias         string
	ModelGroup       string
	Calls, Failures  int
	CostUSD          string
	Unpriced         int
	PromptTokens     int64
	CompletionTokens int64
}

// UsageFilter bounds a usage query. From is included and To excluded, both
// taken as dates. An empty EmployeeID means everybody.
type UsageFilter struct {
	From, To   time.Time
	EmployeeID string
}

// Usage is the daily usage snapshot.
type Usage interface {
	// UpsertDay replaces one day's rows with these. Call it inside a
	// transaction: the delete and the inserts are one change.
	UpsertDay(ctx context.Context, day time.Time, rows []UsageRow) error
	// ByEmployee sums the range per employee (unresolved aliases each count
	// as their own line, EmployeeID empty). ModelGroup and Day are unset.
	ByEmployee(ctx context.Context, f UsageFilter) ([]UsageRow, error)
	// ByModel sums the range per model group, biggest cost first.
	ByModel(ctx context.Context, f UsageFilter) ([]UsageRow, error)
	// ByDay sums the range per day, oldest first.
	ByDay(ctx context.Context, f UsageFilter) ([]UsageRow, error)
	// Rows lists the raw rows in the range, oldest first, at most limit.
	Rows(ctx context.Context, f UsageFilter, limit int) ([]UsageRow, error)
	// Days returns the first and last day that have any rows, or ErrNotFound.
	Days(ctx context.Context) (first, last time.Time, err error)
}

// DeviceTokens are what an agent on the api channel authenticates with.
// The plaintext is returned once, at issue, and never stored.
type DeviceTokens interface {
	// Issue mints a token for the device and revokes its earlier ones at
	// once (no grace): a fresh enrolment supersedes whatever was there.
	Issue(ctx context.Context, deviceID string) (plaintext string, err error)
	// Rotate mints a token and lets the earlier live ones work until grace
	// has passed, so an agent that lost the reply is not locked out.
	Rotate(ctx context.Context, deviceID string, grace time.Duration) (plaintext string, err error)
	// Authenticate finds the live token this plaintext names and returns
	// its device, recording the use. ErrNotFound for anything else --
	// unknown, revoked, or past its grace.
	Authenticate(ctx context.Context, plaintext string, now time.Time) (Device, error)
	// Revoke ends every token of the device. It reports how many it ended.
	Revoke(ctx context.Context, deviceID string) (int, error)
	// HasLive reports whether the device holds a usable token.
	HasLive(ctx context.Context, deviceID string, now time.Time) (bool, error)
	// IssuedAt is when the device's newest live token was minted, for the
	// machine page; ErrNotFound when it has none.
	IssuedAt(ctx context.Context, deviceID string) (time.Time, error)
}

// CredentialBundles hold the credentials.zip delivered to machines, by
// employee and epoch, so the device API serves it from here rather than
// from the bucket.
type CredentialBundles interface {
	Put(ctx context.Context, employeeID string, epoch int, zip []byte, etag string) error
	// Live returns the bundle for the employee's current epoch, or
	// ErrNotFound when none has been built for it.
	Live(ctx context.Context, employeeID string) (zip []byte, etag string, err error)
	// LiveETag is Live without the bytes, for the per-request configuration.
	LiveETag(ctx context.Context, employeeID string) (string, error)
	// Purge removes every bundle of the employee: offboarded or deleted.
	Purge(ctx context.Context, employeeID string) error
}
