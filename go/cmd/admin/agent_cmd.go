package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
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

// downloadBinary fetches the agent binary from an http(s) URL. For a PRIVATE
// GitHub release asset, pass a token (repo scope) via --token or GITHUB_TOKEN:
// without it GitHub returns 404 (it hides private repos) or an HTML login page.
// A cross-host redirect to the signed asset URL is followed by the default
// client, which strips the Authorization header on the way (as it should).
func downloadBinary(url, token string) ([]byte, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("--url must be an http(s) URL, got %q", url)
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized) && token == "" {
			hint = " (private release? pass a GitHub token via --token or GITHUB_TOKEN, repo scope)"
		}
		return nil, fmt.Errorf("download %s: HTTP %d%s", url, resp.StatusCode, hint)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		return nil, fmt.Errorf("download returned HTML, not a binary (Content-Type %q) -- likely an auth wall; pass a token", ct)
	}
	return io.ReadAll(resp.Body)
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
