package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ghrelease"
)

func cmdAgent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin agent <publish <path>|--url <url> --version <v> | cancel | status>")
	}
	switch args[0] {
	case "publish":
		return cmdAgentPublish(args[1:])
	case "cancel":
		return cmdAgentCancel()
	case "status":
		return cmdAgentStatus()
	default:
		return fmt.Errorf("unknown agent subcommand %q (want publish|cancel|status)", args[0])
	}
}

func cmdAgentPublish(args []string) error {
	path, url, version := "", "", ""
	token := os.Getenv("GITHUB_TOKEN")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version":
			if i+1 >= len(args) {
				return fmt.Errorf("--version needs a value")
			}
			version = args[i+1]
			i++
		case "--url":
			if i+1 >= len(args) {
				return fmt.Errorf("--url needs a value")
			}
			url = args[i+1]
			i++
		case "--token":
			if i+1 >= len(args) {
				return fmt.Errorf("--token needs a value")
			}
			token = args[i+1]
			i++
		default:
			if path != "" {
				return fmt.Errorf("unexpected argument %q", args[i])
			}
			path = args[i]
		}
	}
	if version == "" || (path == "" && url == "") {
		return fmt.Errorf("usage: admin agent publish <path> | --url <url> --version <v> [--token <github-pat>]")
	}
	if path != "" && url != "" {
		return fmt.Errorf("give a local path OR --url, not both")
	}

	var data []byte
	var err error
	if url != "" {
		fmt.Printf("downloading %s ...\n", url)
		if data, err = downloadBinary(url, token); err != nil {
			return err
		}
	} else if data, err = os.ReadFile(path); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	mgr, err := newManager()
	if err != nil {
		return err
	}
	sum, err := mgr.PublishAgentUpdate(version, data)
	if err != nil {
		return err
	}
	fmt.Printf("published agent %s (%d bytes, sha256 %s)\n", version, len(data), sum)
	fmt.Println("every machine updates on its next sync, ONE at a time is not possible")
	fmt.Println("(the policy is global) -- validate on one machine first; there is no auto-rollback.")
	fmt.Println("kill switch: admin agent cancel")
	return nil
}

// downloadBinary fetches a release asset, resolving a browser-style release
// URL through the API so a private repository works with a token.
func downloadBinary(url, token string) ([]byte, error) {
	return ghrelease.Fetch(url, token, 30*time.Minute)
}

func cmdAgentCancel() error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	if err := mgr.CancelAgentUpdate(); err != nil {
		return err
	}
	fmt.Println("update target cleared; agents that have not updated yet will stop trying")
	return nil
}

func cmdAgentStatus() error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	p, err := mgr.CurrentPolicy()
	if err != nil {
		return err
	}
	if p.AgentUpdateVersion == "" {
		fmt.Println("no agent update targeted (agents stay on their current version)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.StripEscape) // strip colour Escape bytes
	fmt.Fprintf(w, "%s\t%s\n", cell("TARGET VERSION", cBold), p.AgentUpdateVersion)
	fmt.Fprintf(w, "%s\t%s\n", cell("BINARY SHA256", cBold), p.AgentUpdateSHA256)
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\ncompare against 'admin status' -- machines still on an older version have not updated yet")
	return nil
}
