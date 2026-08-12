package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
)

func cmdMachine(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin machine <bind|unbind|forget|list> ...")
	}
	mgr, err := newManager()
	if err != nil {
		return err
	}

	switch args[0] {
	case "bind":
		if len(args) < 2 {
			return fmt.Errorf("usage: admin machine bind <hostname> --user <name> [--note <text>]")
		}
		machine := args[1]
		user, note := "", ""
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--user":
				if i+1 >= len(args) {
					return fmt.Errorf("--user needs a value")
				}
				user = args[i+1]
				i++
			case "--note":
				if i+1 >= len(args) {
					return fmt.Errorf("--note needs a value")
				}
				note = args[i+1]
				i++
			default:
				return fmt.Errorf("unexpected argument %q", args[i])
			}
		}
		if user == "" {
			return fmt.Errorf("usage: admin machine bind <hostname> --user <name> [--note <text>]")
		}
		if err := mgr.BindMachine(machine, user, note); err != nil {
			return err
		}
		fmt.Printf("machine %q is now assigned to %q\n", machine, user)
		fmt.Println("the agent picks this up on its next sync")
		return nil

	case "unbind":
		if len(args) < 2 {
			return fmt.Errorf("usage: admin machine unbind <hostname>")
		}
		if err := mgr.UnbindMachine(args[1]); err != nil {
			return err
		}
		fmt.Printf("machine %q unbound\n", args[1])
		return nil

	case "forget":
		if len(args) < 2 {
			return fmt.Errorf("usage: admin machine forget <hostname>")
		}
		if err := mgr.ForgetMachine(args[1]); err != nil {
			return err
		}
		fmt.Printf("machine %q forgotten (binding + status removed)\n", args[1])
		fmt.Println("if it is still switched on it will reappear on its next sync; forget it after it is decommissioned")
		return nil

	case "list":
		// Roster of assigned machines, each with its current liveness. The
		// state is the same verdict `admin status` shows (OK / STOPPED /
		// SLEEPING / OFFLINE / STALE / NO REPORT ...), so a machine that has
		// reported a shutdown reads as STOPPED here too. CollectMachines also
		// surfaces unbound reporters; this view stays bound-only so it remains
		// the binding roster and not a second copy of `admin status`.
		machines, err := mgr.CollectMachines(freshAfter)
		if err != nil {
			return err
		}
		bound := make([]admincore.MachineState, 0, len(machines))
		for _, m := range machines {
			if m.Bound {
				bound = append(bound, m)
			}
		}
		if len(bound) == 0 {
			fmt.Println("no machines bound yet -- run 'admin status' to see which ones have reported in")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', tabwriter.StripEscape) // strip colour Escape bytes
		fmt.Fprintln(w, "MACHINE\tUSER\tBOUND AT\tNOTE\tSTATE")
		for _, m := range bound {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				m.Machine, m.Binding.User, m.Binding.BoundAt, dashIfEmpty(m.Binding.Note), stateColor(describeState(m)))
		}
		return w.Flush()

	default:
		return fmt.Errorf("unknown machine subcommand %q", args[0])
	}
}
