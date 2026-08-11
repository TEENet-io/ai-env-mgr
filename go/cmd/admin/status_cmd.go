package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/status"
)

// Liveness thresholds. A live agent refreshes its status roughly every minute
// (the heartbeat), so silence is a strong signal -- but cloud desktops sleep
// and get shut down overnight and on weekends, and that silence is expected.
// The two thresholds separate "away, normal" from "quiet too long, look into
// it", so a machine that hibernates every night is not reported as a problem.
const (
	// freshAfter: reported within this window -> the agent is alive (OK).
	freshAfter = 2 * time.Hour
	// investigateAfter: quiet longer than this and it is no longer just an
	// overnight sleep -- a hibernating machine wakes within a day or two, a
	// dead agent never does. This is the line where silence becomes STALE.
	investigateAfter = 3 * 24 * time.Hour
)

func cmdStatus() error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	machines, err := mgr.CollectMachines(freshAfter)
	if err != nil {
		return err
	}
	if len(machines) == 0 {
		fmt.Println("no machines yet -- once an agent starts up it reports itself here")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE\tUSER\tLOCAL USERS\tAGENT\tLAST SYNC\tPOLICY\tBLOCK\tAPPLOCKER\tSTATE")

	problems := 0
	for _, m := range machines {
		st := m.Status

		user := "-"
		if m.Bound {
			user = m.Binding.User
		}

		locals := "-"
		if len(st.LocalUsers) > 0 {
			locals = strings.Join(st.LocalUsers, ",")
		}

		last := "never"
		agent := "-"
		if !m.Missing {
			last = humanAge(status.Age(st))
			agent = dashIfEmpty(st.AgentVersion)
		}

		block, applocker := "-", "-"
		if !m.Missing {
			block = "off"
			if st.BlockEnabled {
				block = "on"
			}
			applocker = dashIfEmpty(st.AppLockerMode)
		}

		state := describeState(m)
		if needsAttention(state) {
			problems++
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Machine, user, locals, agent, last,
			dashIfEmpty(shorten(st.PolicyETag)), block, applocker, state)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Details go after the table so the columns stay readable.
	for _, m := range machines {
		if m.Unbound {
			fmt.Printf("\n  %s has not been assigned. Local accounts: %s\n",
				m.Machine, strings.Join(m.Status.LocalUsers, ", "))
			fmt.Printf("  Assign it with: admin machine bind %s --user <name>\n", m.Machine)
		}
		if m.Disabled {
			fmt.Printf("\n  %s is still bound to %q, who has been disabled.\n",
				m.Machine, m.Binding.User)
			fmt.Println("  Their credentials have been revoked; reassign or decommission the machine:")
			fmt.Printf("  admin machine bind %s --user <name>   (or: admin machine unbind %s)\n",
				m.Machine, m.Machine)
		}
		if m.UserMissing {
			fmt.Printf("\n  %s is bound to %q but that account has no profile there.\n",
				m.Machine, m.Binding.User)
			fmt.Printf("  Local accounts: %s\n", strings.Join(m.Status.LocalUsers, ", "))
		}
		if len(m.ExtraUsers) > 0 {
			fmt.Printf("\n  %s has unexpected accounts: %s\n",
				m.Machine, strings.Join(m.ExtraUsers, ", "))
		}
		switch m.Status.LastEvent {
		case "suspend":
			fmt.Printf("\n  %s reported it was going to sleep %s -- this is expected, not a fault.\n",
				m.Machine, eventWhen(m.Status.LastEventAt))
		case "stopped":
			fmt.Printf("\n  %s reported a clean stop %s (service stopped or machine shut down).\n",
				m.Machine, eventWhen(m.Status.LastEventAt))
		}
		for _, e := range m.Status.Errors {
			fmt.Printf("  %s: %s\n", m.Machine, e)
		}
	}

	fmt.Println()
	if problems == 0 {
		fmt.Printf("%d machine(s), all healthy\n", len(machines))
	} else {
		fmt.Printf("%d machine(s), %d need attention\n", len(machines), problems)
	}
	return nil
}

// describeState reduces the comparison flags to one word for the table.
// The order matters: the most actionable problem wins. Binding problems come
// first because they need attention regardless of liveness; then the liveness
// verdict, which folds in the agent's own last lifecycle event so an expected
// sleep or stop reads as such rather than as a fault.
func describeState(m admincore.MachineState) string {
	switch {
	case m.Missing:
		return "NO REPORT"
	case m.Unbound:
		return "UNBOUND"
	case m.Disabled:
		return "DISABLED USER"
	case m.UserMissing:
		return "USER MISSING"
	}

	// The machine has reported at least once: judge liveness by how long ago,
	// informed by whether it told us it was leaving.
	age := status.Age(m.Status)
	switch m.Status.LastEvent {
	case "stopped":
		// A clean stop is a known state, not an alarm: the agent said goodbye.
		return "STOPPED"
	case "suspend":
		// It said it was sleeping. Trust that until it has been quiet long
		// enough that "never woke up" becomes the more likely explanation.
		if age > investigateAfter {
			return "STALE"
		}
		return "SLEEPING"
	}

	// No lifecycle event: rely on how fresh the last report is.
	switch {
	case age > investigateAfter:
		return "STALE"
	case age > freshAfter:
		// Quiet, but within the normal off-hours window: expected, not a fault.
		return "OFFLINE"
	}

	// Fresh enough that the agent is clearly alive; surface config drift.
	switch {
	case len(m.ExtraUsers) > 0:
		return "EXTRA USERS"
	case len(m.Status.Errors) > 0:
		return fmt.Sprintf("%d ERROR(S)", len(m.Status.Errors))
	default:
		return "OK"
	}
}

// needsAttention reports whether a table state is something the admin should
// act on. Expected-away states (asleep, cleanly stopped, off overnight) are
// not problems; a crash-like silence or a binding fault is.
func needsAttention(state string) bool {
	switch state {
	case "OK", "SLEEPING", "STOPPED", "OFFLINE":
		return false
	default:
		return true
	}
}

// eventWhen renders a lifecycle event timestamp as a human age ("3h ago").
// An empty or unparseable value degrades to "recently" rather than showing a
// misleading absolute time.
func eventWhen(ts string) string {
	if ts == "" {
		return "recently"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "recently"
	}
	d := time.Since(t)
	if d < 0 {
		return "recently"
	}
	return humanAge(d)
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// shorten trims an ETag to something readable; the full value only matters
// when comparing machines, and the prefix is enough for that.
func shorten(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
