package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TEENet-io/airlock/internal/admincore"
	"github.com/TEENet-io/airlock/internal/status"
)

// staleAfter is how long a machine may go without reporting before the admin
// should look into it. It is deliberately larger than the default sync
// interval so one missed cycle does not raise a false alarm.
const staleAfter = 2 * time.Hour

func cmdStatus() error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	machines, err := mgr.CollectMachines(staleAfter)
	if err != nil {
		return err
	}
	if len(machines) == 0 {
		fmt.Println("no machines yet -- once an agent starts up it reports itself here")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE\tUSER\tLOCAL USERS\tLAST SYNC\tPOLICY\tBLOCK\tAPPLOCKER\tSTATE")

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
		if !m.Missing {
			last = humanAge(status.Age(st))
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
		if state != "OK" {
			problems++
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Machine, user, locals, last,
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
// The order matters: the most actionable problem wins.
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
	case m.Stale:
		return "STALE"
	case len(m.ExtraUsers) > 0:
		return "EXTRA USERS"
	case len(m.Status.Errors) > 0:
		return fmt.Sprintf("%d ERROR(S)", len(m.Status.Errors))
	default:
		return "OK"
	}
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
