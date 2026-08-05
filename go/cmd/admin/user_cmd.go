package main

import (
	"fmt"
	"os"
	"text/tabwriter"
)

func cmdUser(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin user <add|disable|enable|list> ...")
	}
	mgr, err := newManager()
	if err != nil {
		return err
	}

	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: admin user add <name> [--codex <email>] [--claude <email>]")
		}
		name := args[1]
		codex, claude, err := parseAccountFlags(args[2:])
		if err != nil {
			return err
		}
		if err := mgr.AddUser(name, codex, claude); err != nil {
			return err
		}
		fmt.Printf("user %q added and enabled\n", name)
		fmt.Printf("next: admin login --user %s --tool all\n", name)
		return nil

	case "disable":
		if len(args) < 2 {
			return fmt.Errorf("usage: admin user disable <name>")
		}
		if err := mgr.SetUserEnabled(args[1], false); err != nil {
			return err
		}
		fmt.Printf("user %q disabled: no further policy or credentials will be sent\n", args[1])
		return nil

	case "enable":
		if len(args) < 2 {
			return fmt.Errorf("usage: admin user enable <name>")
		}
		if err := mgr.SetUserEnabled(args[1], true); err != nil {
			return err
		}
		fmt.Printf("user %q enabled\n", args[1])
		return nil

	case "list":
		us, err := mgr.LoadUsers()
		if err != nil {
			return err
		}
		if len(us.Users) == 0 {
			fmt.Println("no users yet -- add one with: admin user add <name>")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "USER\tCODEX\tCLAUDE\tENABLED")
		for _, u := range us.Users {
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\n",
				u.WindowsUser, dashIfEmpty(u.CodexAccount), dashIfEmpty(u.ClaudeAccount), u.Enabled)
		}
		return w.Flush()

	default:
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
}

// parseAccountFlags reads the optional --codex / --claude pairs.
// These are notes for the administrator; nothing signs in with them.
func parseAccountFlags(args []string) (codex, claude string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--codex":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--codex needs a value")
			}
			codex = args[i+1]
			i++
		case "--claude":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--claude needs a value")
			}
			claude = args[i+1]
			i++
		default:
			return "", "", fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	return codex, claude, nil
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
