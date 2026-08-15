package main

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
)

func cmdCodex(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin codex <publish <path>|--url <url> --version <v> [--rollout <pct>] | rollout <pct> | cancel | status>")
	}
	switch args[0] {
	case "publish":
		return cmdCodexPublish(args[1:])
	case "rollout":
		return cmdCodexRollout(args[1:])
	case "cancel":
		return cmdCodexCancel()
	case "status":
		return cmdCodexStatus()
	default:
		return fmt.Errorf("unknown codex subcommand %q (want publish|rollout|cancel|status)", args[0])
	}
}

func cmdCodexPublish(args []string) error {
	path, url, version := "", "", ""
	token := os.Getenv("GITHUB_TOKEN")
	// Default to a small first ring rather than the whole fleet: publishing is
	// how a build reaches employees, and this one is a repackaging whose
	// patches drift with every upstream release.
	rollout := 10
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
		case "--rollout":
			if i+1 >= len(args) {
				return fmt.Errorf("--rollout needs a value")
			}
			pct, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fmt.Errorf("--rollout needs a number 0-100, got %q", args[i+1])
			}
			rollout = pct
			i++
		default:
			if path != "" {
				return fmt.Errorf("unexpected argument %q", args[i])
			}
			path = args[i]
		}
	}
	if version == "" || (path == "" && url == "") {
		return fmt.Errorf("usage: admin codex publish <path> --version <v> | --url <url> --version <v> [--token <github-pat>] [--rollout <pct>]")
	}
	if path != "" && url != "" {
		return fmt.Errorf("give a local path OR --url, not both")
	}

	var data []byte
	var err error
	if url != "" {
		fmt.Printf("downloading %s ...\n", url)
		// Same helper as `admin agent publish`: a PRIVATE GitHub release asset
		// needs a token, or GitHub answers 404 / an HTML login page.
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
	fmt.Printf("uploading %d bytes ...\n", len(data))
	sum, err := mgr.PublishCodexUpdate(version, data, rollout)
	if err != nil {
		return err
	}
	fmt.Printf("published Codex %s (%d bytes, sha256 %s)\n", version, len(data), sum)
	fmt.Printf("rollout: %d%% of machines are eligible; widen with 'admin codex rollout <pct>'\n", rollout)
	fmt.Println("machines install on their next sync, skipping any where Codex is running.")
	fmt.Println("kill switch: admin codex cancel (stops further installs; does not uninstall)")
	return nil
}

func cmdCodexRollout(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin codex rollout <pct>")
	}
	pct, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("rollout needs a number 0-100, got %q", args[0])
	}
	mgr, err := newManager()
	if err != nil {
		return err
	}
	if err := mgr.SetCodexRollout(pct); err != nil {
		return err
	}
	fmt.Printf("rollout set to %d%%\n", pct)
	if pct < 100 {
		fmt.Println("(narrowing does not uninstall: machines already updated stay on the new version)")
	}
	return nil
}

func cmdCodexCancel() error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	if err := mgr.CancelCodexUpdate(); err != nil {
		return err
	}
	fmt.Println("Codex target cleared; machines that have not installed it yet will stop trying")
	fmt.Println("machines that already installed it keep it -- to go back, publish the previous version")
	return nil
}

func cmdCodexStatus() error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	p, err := mgr.CurrentPolicy()
	if err != nil {
		return err
	}
	if p.CodexVersion == "" {
		fmt.Println("no Codex version targeted (machines keep whatever they have)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.StripEscape) // strip colour Escape bytes
	fmt.Fprintf(w, "%s\t%s\n", cell("TARGET VERSION", cBold), p.CodexVersion)
	fmt.Fprintf(w, "%s\t%s\n", cell("INSTALLER SHA256", cBold), p.CodexSHA256)
	fmt.Fprintf(w, "%s\t%s\n", cell("INSTALLER KEY", cBold), p.CodexKey)
	fmt.Fprintf(w, "%s\t%d%%\n", cell("ROLLOUT", cBold), p.CodexRolloutPct)
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\ncompare against 'admin machine list' -- machines on another version have not installed it yet")
	return nil
}
