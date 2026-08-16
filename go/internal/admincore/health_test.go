package admincore

import (
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func stamp(ago time.Duration) string {
	return time.Now().Add(-ago).UTC().Format(time.RFC3339)
}

// The states that matter most are the ones telling an operator whether silence
// is expected. An agent that announced it was suspending is not a fault; one
// that simply stopped answering for three days is.
func TestHealth(t *testing.T) {
	bound := model.Binding{User: "work1"}
	cases := []struct {
		name string
		m    MachineState
		want Health
	}{
		{"fresh", MachineState{Bound: true, Binding: bound,
			Status: model.Status{LastSync: stamp(time.Minute)}}, HealthOK},
		{"quiet for an evening", MachineState{Bound: true, Binding: bound,
			Status: model.Status{LastSync: stamp(2 * time.Hour)}}, HealthOffline},
		{"quiet for a week", MachineState{Bound: true, Binding: bound,
			Status: model.Status{LastSync: stamp(7 * 24 * time.Hour)}}, HealthStale},
		{"said it was suspending", MachineState{Bound: true, Binding: bound,
			Status: model.Status{LastSync: stamp(6 * time.Hour), LastEvent: "suspend"}}, HealthSleeping},
		{"suspended and never woke", MachineState{Bound: true, Binding: bound,
			Status: model.Status{LastSync: stamp(8 * 24 * time.Hour), LastEvent: "suspend"}}, HealthStale},
		{"stopped cleanly", MachineState{Bound: true, Binding: bound,
			Status: model.Status{LastSync: stamp(3 * time.Hour), LastEvent: "stopped"}}, HealthStopped},
		{"reporting errors", MachineState{Bound: true, Binding: bound,
			Status: model.Status{LastSync: stamp(time.Minute), Errors: []string{"boom"}}}, HealthErrors},
		{"never reported", MachineState{Bound: true, Binding: bound, Missing: true}, HealthNoReport},
		{"nobody assigned", MachineState{Unbound: true}, HealthUnbound},
		{"employee left", MachineState{Bound: true, Binding: bound, Disabled: true}, HealthDisabledUser},
		{"no profile for the employee", MachineState{Bound: true, Binding: bound, UserMissing: true}, HealthUserMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.Health(); got != c.want {
				t.Fatalf("health = %q, want %q", got, c.want)
			}
		})
	}
}

// An announced sleep or stop must not count against the fleet, or the summary
// would show a wall of red every evening and stop meaning anything.
func TestSeverityTreatsAnnouncedAbsenceAsFine(t *testing.T) {
	for _, h := range []Health{HealthOK, HealthSleeping, HealthStopped} {
		if got := h.Severity(); got != "ok" {
			t.Errorf("%s severity = %q, want ok", h, got)
		}
	}
	for _, h := range []Health{HealthNoReport, HealthDisabledUser, HealthStale, HealthErrors} {
		if got := h.Severity(); got != "bad" {
			t.Errorf("%s severity = %q, want bad", h, got)
		}
	}
	for _, h := range []Health{HealthOffline, HealthUnbound, HealthUserMissing} {
		if got := h.Severity(); got != "warn" {
			t.Errorf("%s severity = %q, want warn", h, got)
		}
	}
}

// A bound machine whose employee has a profile but no credentials yet is
// waiting for somebody to sign in on their behalf. The agent reports that as
// an error, and treating it as one paints an ordinary onboarding step red.
func TestPendingCredentialsIsNotAFault(t *testing.T) {
	bound := model.Binding{User: "weipeng"}
	pending := MachineState{Bound: true, Binding: bound, Status: model.Status{
		LastSync:        stamp(time.Minute),
		BoundUserExists: true,
		CredsETag:       "",
		Errors:          []string{`no credentials published for "weipeng" yet`},
	}}
	if got := pending.Health(); got != HealthCredsPending {
		t.Fatalf("health = %q, want creds_pending", got)
	}
	if got := pending.Health().Severity(); got != "warn" {
		t.Fatalf("severity = %q, want warn", got)
	}

	// A second problem alongside it is something else, and something else is
	// worth the red.
	alsoBroken := pending
	alsoBroken.Status.Errors = append([]string{"policy apply: access denied"}, pending.Status.Errors...)
	if got := alsoBroken.Health(); got != HealthErrors {
		t.Fatalf("a real error was hidden behind pending credentials: %q", got)
	}

	// Once credentials are delivered the machine is simply fine.
	delivered := pending
	delivered.Status.CredsETag = "abc123"
	delivered.Status.Errors = nil
	if got := delivered.Health(); got != HealthOK {
		t.Fatalf("health = %q, want ok", got)
	}
}

// From 1.2.5 the agent keeps states out of Errors, so a machine waiting to be
// signed in for reports no errors at all -- and a machine that does report one
// has something genuinely wrong.
func TestNewAgentSeparatesWarningsFromErrors(t *testing.T) {
	bound := model.Binding{User: "weipeng"}
	pending := MachineState{Bound: true, Binding: bound, Status: model.Status{
		LastSync:        stamp(time.Minute),
		BoundUserExists: true,
		Warnings:        []string{`no credentials published for "weipeng" yet`},
	}}
	if got := pending.Health(); got != HealthCredsPending {
		t.Fatalf("health = %q, want creds_pending", got)
	}

	broken := pending
	broken.Status.Errors = []string{"policy apply: access denied"}
	if got := broken.Health(); got != HealthErrors {
		t.Fatalf("a real failure was not flagged: %q", got)
	}
}

// The platform is asked only about machines whose own report leaves the
// question open. Asking about a machine that reported a minute ago, or that
// said goodbye on its way out, would be a call made for nothing.
func TestOnlySilentMachinesNeedTheCloud(t *testing.T) {
	bound := model.Binding{User: "work1"}
	mk := func(st model.Status) MachineState {
		return MachineState{Bound: true, Binding: bound, Status: st}
	}
	cases := []struct {
		name string
		m    MachineState
		want bool
	}{
		{"reporting normally", mk(model.Status{LastSync: stamp(time.Minute), BoundUserExists: true, CredsETag: "e"}), false},
		{"said it was suspending", mk(model.Status{LastSync: stamp(2 * time.Hour), LastEvent: "suspend"}), false},
		{"stopped cleanly", mk(model.Status{LastSync: stamp(2 * time.Hour), LastEvent: "stopped"}), false},
		{"quiet for an evening", mk(model.Status{LastSync: stamp(2 * time.Hour)}), true},
		{"quiet for a week", mk(model.Status{LastSync: stamp(7 * 24 * time.Hour)}), true},
		{"never reported", MachineState{Bound: true, Binding: bound, Missing: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.NeedsCloudLookup(); got != c.want {
				t.Fatalf("NeedsCloudLookup = %v, want %v (health %q)", got, c.want, c.m.Health())
			}
		})
	}
}

// What the platform says turns one ambiguous silence into four answers. The
// one that matters is a desktop the platform calls Running while its agent
// says nothing -- today that hides inside "offline" and looks like an ordinary
// evening.
func TestCloudStateDisambiguatesSilence(t *testing.T) {
	quiet := MachineState{Bound: true, Binding: model.Binding{User: "work1"},
		Status: model.Status{LastSync: stamp(2 * time.Hour)}}

	cases := []struct {
		name string
		c    CloudDesktop
		want Health
		sev  string
	}{
		// Hibernation is reported through ManagementFlags while Status stays
		// Stopped, which is why reading Status alone would call this shut down.
		{"hibernating", CloudDesktop{Found: true, Status: "Stopped", Hibernated: true}, HealthHibernated, "ok"},
		{"shut down", CloudDesktop{Found: true, Status: "Stopped"}, HealthStopped, "ok"},
		{"running but silent", CloudDesktop{Found: true, Status: "Running"}, HealthAgentDown, "bad"},
		{"no longer exists", CloudDesktop{Found: false}, HealthCloudMissing, "warn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := quiet
			cd := c.c
			m.Cloud = &cd
			if got := m.Health(); got != c.want {
				t.Fatalf("health = %q, want %q", got, c.want)
			}
			if got := m.Health().Severity(); got != c.sev {
				t.Fatalf("severity = %q, want %q", got, c.sev)
			}
		})
	}

	// Without an answer from the platform it stays what it was.
	if got := quiet.Health(); got != HealthOffline {
		t.Fatalf("health without cloud state = %q, want offline", got)
	}
}
