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
