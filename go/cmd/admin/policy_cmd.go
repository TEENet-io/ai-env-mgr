package main

import (
	"fmt"
	"strconv"

	"github.com/TEENet-io/airlock/internal/model"
)

func cmdAddSite(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin add-site <domain>...")
	}
	mgr, err := newManager()
	if err != nil {
		return err
	}
	p, err := mgr.MutateDomains(args, nil)
	if err != nil {
		return err
	}
	printPolicy(p)
	fmt.Println("agents will pick this up on their next sync; browsers need a restart")
	return nil
}

func cmdRemoveSite(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin remove-site <domain>...")
	}
	mgr, err := newManager()
	if err != nil {
		return err
	}
	p, err := mgr.MutateDomains(nil, args)
	if err != nil {
		return err
	}
	printPolicy(p)
	return nil
}

func cmdListSites() error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	// Read-only: this must not rewrite anything.
	p, err := mgr.CurrentPolicy()
	if err != nil {
		return err
	}
	printPolicy(p)
	return nil
}

func cmdSetBlock(enabled bool) error {
	mgr, err := newManager()
	if err != nil {
		return err
	}
	p, err := mgr.SetBlockEnabled(enabled)
	if err != nil {
		return err
	}
	if enabled {
		fmt.Println("blocking ENABLED for all users")
	} else {
		fmt.Println("blocking DISABLED for all users -- remember to re-enable it")
	}
	printPolicy(p)
	return nil
}

func cmdSetInterval(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin set-interval <minutes>")
	}
	minutes, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("interval must be a whole number of minutes: %w", err)
	}
	mgr, err := newManager()
	if err != nil {
		return err
	}
	p, err := mgr.SetSyncInterval(minutes)
	if err != nil {
		return err
	}
	if p.SyncIntervalMinutes != minutes {
		fmt.Printf("requested %d minutes, applied %d (allowed range is %d-%d)\n",
			minutes, p.SyncIntervalMinutes, model.MinSyncInterval, model.MaxSyncInterval)
	} else {
		fmt.Printf("sync interval set to %d minutes\n", p.SyncIntervalMinutes)
	}
	fmt.Println("each agent switches over after its current cycle")
	return nil
}

func printPolicy(p model.Policy) {
	state := "on"
	if !p.BlockEnabled {
		state = "off"
	}
	fmt.Printf("blocking: %s   interval: %dm   domains: %d\n",
		state, p.SyncIntervalMinutes, len(p.BlockedDomains))
	for _, d := range p.BlockedDomains {
		fmt.Println("  -", d)
	}
}
