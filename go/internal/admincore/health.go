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
	HealthCredsPending Health = "creds_pending" // nobody has signed in for the employee yet

	// These four need the platform's answer as well as the agent's silence,
	// and are only reachable when cloud state was looked up.
	HealthHibernated   Health = "hibernated"    // the platform is holding it suspended
	HealthAgentDown    Health = "agent_down"    // powered on, but the agent is not reporting
	HealthCloudMissing Health = "cloud_missing" // no such desktop any more
)

// CloudDesktop is what the platform says about a machine, filled in only when
// the console had reason to ask.
//
// The agent cannot answer this itself: WuYing suspends a desktop at the
// hypervisor, so the guest Windows never sees a power event and the agent gets
// no chance to say it is going away. Silence therefore looks identical whether
// a machine is asleep for the night or its agent has died -- and those want
// very different responses.
type CloudDesktop struct {
	DesktopID  string
	Status     string // Running, Stopped, Starting, ...
	Hibernated bool   // from ManagementFlags, NOT Status: a hibernating desktop reports Stopped
	Found      bool   // false means the lookup ran and matched nothing
}

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

	if age > FreshAfter {
		// Quiet. The platform, when asked, turns one ambiguous silence into
		// four distinct answers.
		if c := m.Cloud; c != nil {
			switch {
			case !c.Found:
				return HealthCloudMissing
			case c.Hibernated:
				return HealthHibernated
			case c.Status == "Running":
				// Powered on and reachable by the platform, yet saying
				// nothing: this is the case that used to hide inside
				// "offline", and the only one here that needs somebody.
				return HealthAgentDown
			case c.Status == "Stopped":
				return HealthStopped
			}
		}
		if age > InvestigateAfter {
			return HealthStale
		}
		// Quiet, but within the normal off-hours window: expected, not a fault.
		return HealthOffline
	}

	// A real failure outranks everything below: something has to be done.
	//
	// Agents from 1.2.5 on keep states out of Errors entirely. Older ones put
	// them there, so the count is tolerated for the one entry such an agent
	// adds for pending credentials -- without that, every machine waiting to be
	// signed in for would show red until its agent is updated.
	// An agent that separates the two always has a warning to show for pending
	// credentials, so an empty Warnings list is what identifies the old one.
	// Without that check, a new agent reporting a genuine failure alongside
	// pending credentials would be read as merely pending.
	oldAgentCredsNotice := m.Bound && m.Status.BoundUserExists && m.Status.CredsETag == "" &&
		len(m.Status.Errors) == 1 && len(m.Status.Warnings) == 0
	if len(m.Status.Errors) > 0 && !oldAgentCredsNotice {
		return HealthErrors
	}

	// Bound, the employee has a profile, and nobody has signed in for them
	// yet: mid-onboarding rather than broken. CredsETag is the structural
	// signal -- set on every path where credentials were found, empty only
	// when none are published -- so this does not depend on the wording of a
	// message.
	if m.Bound && m.Status.BoundUserExists && m.Status.CredsETag == "" {
		return HealthCredsPending
	}
	return HealthOK
}

// Severity groups the states into the three-way vocabulary a summary needs:
// "fine", "worth a look", "someone has to act".
func (h Health) Severity() string {
	switch h {
	case HealthOK, HealthSleeping, HealthStopped, HealthHibernated:
		// A machine that announced it was sleeping or stopping is doing what
		// it was told; counting it as a fault would make the summary cry wolf
		// every evening.
		return "ok"
	case HealthNoReport, HealthDisabledUser, HealthErrors, HealthStale, HealthAgentDown:
		return "bad"
	case HealthCloudMissing:
		return "warn"
	case HealthCredsPending:
		// Setup not finished, like unassigned or no profile -- the same amber
		// as its siblings, not the red reserved for something going wrong.
		return "warn"
	default:
		return "warn"
	}
}
