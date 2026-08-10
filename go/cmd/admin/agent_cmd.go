package main

import (
	"fmt"
	"os"
)

func cmdAgent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin agent <publish <path> --version <v> | cancel | status>")
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
	path, version := "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version":
			if i+1 >= len(args) {
				return fmt.Errorf("--version needs a value")
			}
			version = args[i+1]
			i++
		default:
			if path != "" {
				return fmt.Errorf("unexpected argument %q", args[i])
			}
			path = args[i]
		}
	}
	if path == "" || version == "" {
		return fmt.Errorf("usage: admin agent publish <path-to-agent.exe> --version <v>")
	}
	data, err := os.ReadFile(path)
	if err != nil {
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
	fmt.Printf("target version: %s\n", p.AgentUpdateVersion)
	fmt.Printf("binary sha256:  %s\n", p.AgentUpdateSHA256)
	fmt.Println("compare against 'admin status' -- machines still on an older version have not updated yet")
	return nil
}
