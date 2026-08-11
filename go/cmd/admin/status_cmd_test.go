package main

import (
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// reportedAgo builds a machine that reported `age` ago with the given lifecycle
// event, so the liveness logic in describeState can be exercised without a store.
func reportedAgo(age time.Duration, event string) admincore.MachineState {
	return admincore.MachineState{
		Machine: "DESKTOP-A",
		Bound:   true,
		Status: model.Status{
			Machine:   "DESKTOP-A",
			BoundUser: "work1",
			LastSync:  time.Now().UTC().Add(-age).Format(time.RFC3339),
			LastEvent: event,
		},
	}
}

func TestDescribeStateGraduatedLiveness(t *testing.T) {
	cases := []struct {
		name  string
		age   time.Duration
		event string
		want  string
	}{
		{"fresh is OK", 5 * time.Minute, "", "OK"},
		{"quiet a few hours is OFFLINE", 6 * time.Hour, "", "OFFLINE"},
		{"quiet for days is STALE", 5 * 24 * time.Hour, "", "STALE"},
		{"suspend marker reads as SLEEPING", 6 * time.Hour, "suspend", "SLEEPING"},
		{"suspend that never woke escalates to STALE", 5 * 24 * time.Hour, "suspend", "STALE"},
		{"clean stop reads as STOPPED", 6 * time.Hour, "stopped", "STOPPED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeState(reportedAgo(tc.age, tc.event)); got != tc.want {
				t.Errorf("describeState(age=%v event=%q) = %q, want %q", tc.age, tc.event, got, tc.want)
			}
		})
	}
}

// Expected-away states must not inflate the "needs attention" count; a machine
// that hibernates every night is not a problem to chase.
func TestNeedsAttentionIgnoresExpectedAwayStates(t *testing.T) {
	quiet := []string{"OK", "SLEEPING", "STOPPED", "OFFLINE"}
	for _, s := range quiet {
		if needsAttention(s) {
			t.Errorf("%q should not need attention", s)
		}
	}
	problems := []string{"STALE", "NO REPORT", "UNBOUND", "DISABLED USER", "USER MISSING", "EXTRA USERS", "1 ERROR(S)"}
	for _, s := range problems {
		if !needsAttention(s) {
			t.Errorf("%q should need attention", s)
		}
	}
}

// Binding faults outrank liveness: a machine bound to an offboarded employee
// needs attention even while it is asleep.
func TestDescribeStateBindingFaultsOutrankLiveness(t *testing.T) {
	m := reportedAgo(6*time.Hour, "suspend")
	m.Disabled = true
	if got := describeState(m); got != "DISABLED USER" {
		t.Errorf("describeState = %q, want DISABLED USER", got)
	}
}
