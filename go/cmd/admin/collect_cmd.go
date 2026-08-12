package main

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func cmdCollect(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin collect <enable|disable|stat> ...")
	}
	switch args[0] {
	case "enable":
		return cmdCollectEnable(args[1:])
	case "disable":
		return cmdCollectSet(false, nil, nil)
	case "stat":
		return cmdCollectStat()
	default:
		return fmt.Errorf("unknown collect subcommand %q (want enable|disable|stat)", args[0])
	}
}

func cmdCollectEnable(args []string) error {
	var since *string
	var quiet *int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--since":
			if i+1 >= len(args) {
				return fmt.Errorf("--since needs a value (YYYY-MM-DD)")
			}
			v := args[i+1]
			if _, err := time.Parse("2006-01-02", v); err != nil {
				return fmt.Errorf("--since must be YYYY-MM-DD, got %q", v)
			}
			since = &v
			i++
		case "--quiet":
			if i+1 >= len(args) {
				return fmt.Errorf("--quiet needs a value (seconds)")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return fmt.Errorf("--quiet must be a non-negative number of seconds, got %q", args[i+1])
			}
			quiet = &n
			i++
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	return cmdCollectSet(true, since, quiet)
}

func cmdCollectSet(enabled bool, since *string, quiet *int) error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	p, err := mgr.SetCollect(enabled, since, quiet)
	if err != nil {
		return err
	}
	printCollectPolicy(p)
	if enabled {
		fmt.Println("collection ENABLED -- the agent RAM policy must grant PutObject on")
		fmt.Println("  <bucket>/agent_workdir/*/data_collect/*, or uploads are denied")
	} else {
		fmt.Println("collection DISABLED")
	}
	fmt.Println("agents switch over after their current cycle")
	return nil
}

func printCollectPolicy(p model.Policy) {
	state := "off"
	if p.CollectEnabled {
		state = "on"
	}
	quiet := p.CollectQuietSeconds
	if quiet <= 0 {
		quiet = 60
	}
	since := p.CollectSince
	if since == "" {
		since = "(all history)"
	}
	fmt.Printf("collection: %s   quiet: %ds   since: %s\n", state, quiet, since)
}

func cmdCollectStat() error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	stats, enabled, err := mgr.CollectStats()
	if err != nil {
		return err
	}
	if len(stats) == 0 {
		fmt.Println("nothing collected yet, and no employees on the roster -- add one with 'admin user add'")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tOBJECTS\t.claude\t.codex\tLATEST")
	withData, total := 0, 0
	for _, s := range stats {
		latest := "-"
		if !s.Latest.IsZero() {
			latest = humanAge(time.Since(s.Latest))
		}
		if s.Total > 0 {
			withData++
		}
		total += s.Total
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", s.User, s.Total, s.Claude, s.Codex, latest)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\ntotal: %d objects, %d/%d users have data\n", total, withData, len(stats))
	state := "off"
	if enabled {
		state = "on"
	}
	fmt.Printf("collectEnabled = %s\n", state)
	if !enabled {
		fmt.Println("(collection is OFF -- any counts above are data uploaded while it was on)")
	}
	return nil
}
