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

// Liveness thresholds. A live agent re-writes its status roughly every minute
// -- the loop ticks every minute and, when a full sync is not due, sends a
// heartbeat that refreshes "last seen" regardless of the (possibly long) sync
// interval. So a running machine's report is never more than a minute or two
// old, which makes silence a strong, fast signal.
// The thresholds live in admincore alongside the rules that use them; these
// aliases keep the call sites here readable.
const (
	freshAfter       = admincore.FreshAfter
	investigateAfter = admincore.InvestigateAfter
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

	// StripEscape: colourised STATE cells wrap their ANSI codes in the Escape
	// byte so tabwriter ignores them for width; this flag then removes those
	// bytes from the output. Without it they leak as 0xff garbage.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.StripEscape)
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
			dashIfEmpty(shorten(st.PolicyETag)), block, applocker, stateColor(state))
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
		fmt.Printf("%d machine(s), %s\n", len(machines), paint("all healthy", cGreen))
	} else {
		fmt.Printf("%d machine(s), %s\n", len(machines), paint(fmt.Sprintf("%d need attention", problems), cYellow))
	}
	return nil
}

// describeState reduces the comparison flags to one word for the table.
// The order matters: the most actionable problem wins. Binding problems come
// first because they need attention regardless of liveness; then the liveness
// verdict, which folds in the agent's own last lifecycle event so an expected
// sleep or stop reads as such rather than as a fault.
// describeState renders the shared classification with the terminal's labels.
// The rules themselves live in admincore.Health so the web console cannot
// drift from them.
func describeState(m admincore.MachineState) string {
	switch h := m.Health(); h {
	case admincore.HealthNoReport:
		return "NO REPORT"
	case admincore.HealthUnbound:
		return "UNBOUND"
	case admincore.HealthDisabledUser:
		return "DISABLED USER"
	case admincore.HealthUserMissing:
		return "USER MISSING"
	case admincore.HealthStopped:
		return "STOPPED"
	case admincore.HealthSleeping:
		return "SLEEPING"
	case admincore.HealthStale:
		return "STALE"
	case admincore.HealthOffline:
		return "OFFLINE"
	case admincore.HealthErrors:
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
