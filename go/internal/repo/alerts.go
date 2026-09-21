package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Alert kinds. Each is one rule; the fingerprint names the one thing the
// rule saw, so the same condition seen again is the same alert.
const (
	AlertMachineOffline = "machine_offline"
	AlertRolloutFailed  = "rollout_failed"
	AlertGatewayDrift   = "gateway_drift"
	AlertBudget         = "budget"
	AlertTaskFailed     = "task_failed"

	SeverityWarn = "warn"
	SeverityCrit = "crit"
)

// Alert is one condition and what has happened to it since.
type Alert struct {
	ID          string
	Kind        string
	Fingerprint string
	Severity    string
	SubjectType string // device | employee | grant | task | target
	SubjectID   string
	Title       string
	Detail      string
	OpenedAt    time.Time
	ResolvedAt  *time.Time
	ResolvedBy  string
	AckedAt     *time.Time
	AckedBy     string
	NotifiedAt  *time.Time
	NotifyTries int
	NotifyError string
}

// Open reports whether the condition still stands.
func (a Alert) Open() bool { return a.ResolvedAt == nil }

type NewAlert struct {
	Kind, Fingerprint, Severity string
	SubjectType, SubjectID      string
	Title, Detail               string
}

// Alerts is the alert log.
type Alerts interface {
	// Open records the condition unless the same fingerprint is already
	// open, in which case the open one is returned and created is false.
	Open(ctx context.Context, a NewAlert) (alert Alert, created bool, err error)
	// Resolve closes the open alert with this fingerprint. It reports how
	// many it closed: zero is fine, the condition was never open.
	Resolve(ctx context.Context, fingerprint, by string) (int, error)
	ResolveByID(ctx context.Context, id, by string) (Alert, error)
	Ack(ctx context.Context, id, by string) (Alert, error)
	ByID(ctx context.Context, id string) (Alert, error)
	ListOpen(ctx context.Context) ([]Alert, error)              // newest first
	ListRecent(ctx context.Context, limit int) ([]Alert, error) // open and closed, newest first
	// Unnotified lists open alerts nobody has been told about yet, oldest
	// first, skipping those that have failed to send more than maxTries.
	Unnotified(ctx context.Context, maxTries, limit int) ([]Alert, error)
	// MarkNotified records a delivery attempt: at with an empty error means
	// sent, otherwise the error is kept and the try counted.
	MarkNotified(ctx context.Context, id string, at time.Time, errText string) error
	CountOpen(ctx context.Context) (int, error)
}

// SettingAlerts holds AlertSettings as JSON.
const SettingAlerts = "alerts"

// AlertSettings is what the rules compare against.
type AlertSettings struct {
	Enabled           bool `json:"enabled"`
	OfflineAfterHours int  `json:"offline_after_hours"`
	BudgetWarnPercent int  `json:"budget_warn_percent"`
}

// DefaultAlertSettings is what applies until an administrator changes it:
// on, a day of silence, a warning at four fifths of the budget.
func DefaultAlertSettings() AlertSettings {
	return AlertSettings{Enabled: true, OfflineAfterHours: 24, BudgetWarnPercent: 80}
}

// Validate rejects thresholds that would fire always or never.
func (s AlertSettings) Validate() error {
	if s.OfflineAfterHours < 1 || s.OfflineAfterHours > 24*30 {
		return errors.New("offline threshold must be between 1 hour and 30 days")
	}
	if s.BudgetWarnPercent < 1 || s.BudgetWarnPercent > 99 {
		return errors.New("budget warning must be between 1% and 99%")
	}
	return nil
}

// LoadAlertSettings reads the stored settings, or the defaults with version
// 0 when nothing is stored yet.
func LoadAlertSettings(ctx context.Context, settings Settings) (AlertSettings, int, error) {
	setting, err := settings.Get(ctx, SettingAlerts)
	if errors.Is(err, ErrNotFound) {
		return DefaultAlertSettings(), 0, nil
	}
	if err != nil {
		return AlertSettings{}, 0, err
	}
	var s AlertSettings
	if err := json.Unmarshal(setting.Value, &s); err != nil {
		return AlertSettings{}, 0, fmt.Errorf("the stored alert settings are not readable: %w", err)
	}
	return s, setting.Version, nil
}

// SettingAlertChannels holds ChannelSettings as JSON.
const SettingAlertChannels = "alert_channels"

// Sealed is a secret at rest: envelope-encrypted with the master key, with
// the key version that wrapped it. An empty Ciphertext means nothing is set.
type Sealed struct {
	Ciphertext []byte `json:"ciphertext,omitempty"`
	KeyVersion string `json:"key_version,omitempty"`
}

// IsSet reports whether a secret has been stored.
func (s Sealed) IsSet() bool { return len(s.Ciphertext) > 0 }

// WebhookSetting is one HTTP endpoint that gets a JSON body per alert.
type WebhookSetting struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Format  string `json:"format"` // generic | dingtalk | feishu
	Secret  Sealed `json:"secret"` // signing secret, where the format has one
}

// SMTPSetting is one mail relay and its recipients.
type SMTPSetting struct {
	Enabled  bool     `json:"enabled"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Password Sealed   `json:"password"`
	From     string   `json:"from"`
	To       []string `json:"to"`
}

// ChannelSettings is where alerts go.
type ChannelSettings struct {
	Webhook WebhookSetting `json:"webhook"`
	SMTP    SMTPSetting    `json:"smtp"`
}

// LoadChannelSettings reads the stored channels, or empty ones with version
// 0 when nothing is stored yet.
func LoadChannelSettings(ctx context.Context, settings Settings) (ChannelSettings, int, error) {
	setting, err := settings.Get(ctx, SettingAlertChannels)
	if errors.Is(err, ErrNotFound) {
		return ChannelSettings{}, 0, nil
	}
	if err != nil {
		return ChannelSettings{}, 0, err
	}
	var s ChannelSettings
	if err := json.Unmarshal(setting.Value, &s); err != nil {
		return ChannelSettings{}, 0, fmt.Errorf("the stored alert channels are not readable: %w", err)
	}
	return s, setting.Version, nil
}

// SettingRotation holds RotationSettings as JSON.
const SettingRotation = "rotation"

// RotationSettings says when an employee's gateway token is replaced on its
// own. Off until an administrator turns it on: a fleet that has never seen
// a re-issue should see the first one on purpose.
type RotationSettings struct {
	Enabled bool `json:"enabled"`
	// MaxAgeDays is how old a live token may get before it is replaced.
	MaxAgeDays int `json:"max_age_days"`
	// PerDay bounds one night's re-issues, so a fleet whose tokens were all
	// minted the same week does not all change on the same night.
	PerDay int `json:"per_day"`
	// ActiveWithinHours: only an employee whose machine reported this
	// recently is rotated. The old token is revoked as soon as the new one
	// is minted, and a machine that is not syncing would never receive it.
	ActiveWithinHours int `json:"active_within_hours"`
}

func DefaultRotationSettings() RotationSettings {
	return RotationSettings{Enabled: false, MaxAgeDays: 90, PerDay: 5, ActiveWithinHours: 24}
}

func (s RotationSettings) Validate() error {
	if s.MaxAgeDays < 7 || s.MaxAgeDays > 730 {
		return errors.New("token age must be between 7 and 730 days")
	}
	if s.PerDay < 1 || s.PerDay > 100 {
		return errors.New("per-day limit must be between 1 and 100")
	}
	if s.ActiveWithinHours < 1 || s.ActiveWithinHours > 24*14 {
		return errors.New("the activity window must be between 1 hour and 14 days")
	}
	return nil
}

func LoadRotationSettings(ctx context.Context, settings Settings) (RotationSettings, int, error) {
	setting, err := settings.Get(ctx, SettingRotation)
	if errors.Is(err, ErrNotFound) {
		return DefaultRotationSettings(), 0, nil
	}
	if err != nil {
		return RotationSettings{}, 0, err
	}
	var s RotationSettings
	if err := json.Unmarshal(setting.Value, &s); err != nil {
		return RotationSettings{}, 0, fmt.Errorf("the stored rotation settings are not readable: %w", err)
	}
	return s, setting.Version, nil
}
