package admincore

import (
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/status"
)

// Health is what a machine's report says about it. The two front ends label
// these differently, but neither decides them: the rules live here so the CLI
// and the console can never disagree about whether a machine is in trouble.
type Health string

const (
	HealthOK           Health = "ok"
	HealthErrors       Health = "errors"        // alive, but reporting problems
	HealthOffline      Health = "offline"       // quiet, but not for long
	HealthSleeping     Health = "sleeping"      // said it was suspending
	HealthStopped      Health = "stopped"       // said goodbye cleanly
	HealthStale        Health = "stale"         // quiet long enough to investigate
	HealthNoReport     Health = "no_report"     // bound, never reported at all
	HealthUnbound      Health = "unbound"       // reported, assigned to nobody
	HealthDisabledUser Health = "disabled_user" // bound to someone offboarded
	HealthUserMissing  Health = "user_missing"  // the bound user has no profile
)

// Liveness thresholds.
//
// The agent ticks every minute and heartbeats even when a full sync is not
// due, so a running machine is never more than a minute or two stale. That
// makes silence a fast, strong signal.
const (
	// FreshAfter: reported within this window and the agent is clearly alive.
	// Ten minutes is ten missed heartbeats -- past any network blip, but far
	// tighter than a sync interval, so a machine that is genuinely off shows
	// as such within minutes rather than looking healthy for hours.
	FreshAfter = 10 * time.Minute
	// InvestigateAfter: quiet longer than this and it is no longer an
	// overnight sleep or a weekend. A hibernating machine wakes within a day
	// or two; a dead agent never does.
	InvestigateAfter = 3 * 24 * time.Hour
)

// Health judges one machine.
func (m MachineState) Health() Health {
	switch {
	case m.Missing:
		return HealthNoReport
	case m.Unbound:
		return HealthUnbound
	case m.Disabled:
		return HealthDisabledUser
	case m.UserMissing:
		return HealthUserMissing
	}

	// It has reported at least once: judge liveness by how long ago, informed
	// by whether it told us it was going away.
	age := status.Age(m.Status)
	switch m.Status.LastEvent {
	case "stopped":
		// A clean stop is a known state, not an alarm: the agent said goodbye.
		return HealthStopped
	case "suspend":
		// Trust "I am sleeping" until it has been quiet long enough that
		// "never woke up" is the better explanation.
		if age > InvestigateAfter {
			return HealthStale
		}
		return HealthSleeping
	}

	switch {
	case age > InvestigateAfter:
		return HealthStale
	case age > FreshAfter:
		// Quiet, but within the normal off-hours window: expected, not a fault.
		return HealthOffline
	}
	if len(m.Status.Errors) > 0 {
		return HealthErrors
	}
	return HealthOK
}

// Severity groups the states into the three-way vocabulary a summary needs:
// "fine", "worth a look", "someone has to act".
func (h Health) Severity() string {
	switch h {
	case HealthOK, HealthSleeping, HealthStopped:
		// A machine that announced it was sleeping or stopping is doing what
		// it was told; counting it as a fault would make the summary cry wolf
		// every evening.
		return "ok"
	case HealthNoReport, HealthDisabledUser, HealthErrors, HealthStale:
		return "bad"
	default:
		return "warn"
	}
}
