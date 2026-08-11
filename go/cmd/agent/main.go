// Command agent runs on each cloud desktop as a Windows service. It pulls the
// website block policy and the AI tool credentials from OSS, applies them
// locally, and reports back what it did.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/winsvc"
)

var version = "dev"

const (
	serviceName = "AIEnvMgrAgent"
	serviceDisp = "AI Env Mgr Agent"
	serviceDesc = "Applies AI Env Mgr policy and AI tool credentials from the configuration store."

	// minSyncGap debounces the three triggers (startup, wake-up, overdue check)
	// so a machine that resumes right after booting does not sync twice.
	minSyncGap = 30 * time.Second
)

func stateDir() string {
	if v := os.Getenv("ProgramData"); v != "" {
		return filepath.Join(v, "AIEnvMgr")
	}
	return filepath.FromSlash("/var/lib/ai-env-mgr")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "setup":
		must(cmdSetup())
	case "install":
		must(winsvc.Install(serviceName, serviceDisp, serviceDesc))
		fmt.Println("service installed")
	case "uninstall":
		must(winsvc.Uninstall(serviceName))
		fmt.Println("service removed")
	case "start":
		must(winsvc.Start(serviceName))
		fmt.Println("service started")
	case "stop":
		must(winsvc.Stop(serviceName))
		fmt.Println("service stopped")
	case "sync":
		st, err := runOnce()
		must(err)
		printStatus(st, true)
	case "status":
		rep, err := readLocalState()
		must(err)
		printLocalState(rep)
	case "run":
		runService()
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
}

func usage() {
	fmt.Println(`agent.exe -- AI Env Mgr agent

  setup       do everything at once: copy into place, lock down permissions,
              register and start the service, then verify. Run this once on
              the template machine, as Administrator.

  install     register as an auto-start Windows service
  uninstall   remove the service
  start|stop  control the service
  sync        run one sync cycle now (for verification and troubleshooting)
  status      show what this machine currently has applied -- read-only,
              changes nothing and contacts nothing
  run         run the service body in the foreground (used by the service
              manager; not normally invoked by hand)
  version     print the agent version`)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func printStatus(st model.Status, verbose bool) {
	// Worth printing: a shipped agent has its settings written into the
	// binary, so someone editing agent.config.json and seeing nothing
	// change needs to be told why.
	fmt.Printf("config=%s\n", config.Source(builtIn()))
	fmt.Printf("machine=%s\n", st.Machine)
	if st.BoundUser == "" {
		fmt.Println("bound to: (nothing yet -- ask the administrator to bind this machine)")
	} else {
		exists := "profile present"
		if !st.BoundUserExists {
			exists = "NO PROFILE ON THIS MACHINE"
		}
		fmt.Printf("bound to: %s (%s)\n", st.BoundUser, exists)
	}
	fmt.Printf("local users: %s\n", strings.Join(st.LocalUsers, ", "))
	fmt.Printf("block=%v domains=%d interval=%dm creds=%v applocker=%s\n",
		st.BlockEnabled, st.BlockedDomains, st.SyncIntervalMinutes, st.CredsApplied, st.AppLockerMode)
	fmt.Printf("collect=%v uploaded=%d\n", st.CollectEnabled, st.CollectUploaded)
	if verbose {
		fmt.Printf("policyEtag=%s credsEtag=%s\n", orDash(st.PolicyETag), orDash(st.CredsETag))
	}
	if len(st.Errors) > 0 {
		fmt.Printf("errors (%d):\n", len(st.Errors))
		for _, e := range st.Errors {
			fmt.Println("  -", e)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// newSyncer wires the real OSS store and the real local applier together.
func newSyncer() (*agentcore.Syncer, error) {
	cfg, _, err := config.Resolve(builtIn(), config.DefaultAgentPath())
	if err != nil {
		return nil, err
	}
	store, err := ossclient.New(cfg.Endpoint, cfg.Bucket, cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, err
	}
	machine, err := newLocalMachine()
	if err != nil {
		return nil, err
	}
	return &agentcore.Syncer{
		Store:            store,
		Applier:          localApplier{},
		Machine:          machine,
		Version:          version,
		StateDir:         stateDir(),
		FallbackInterval: cfg.IntervalMinutes,
		Collector: &agentcore.Collector{
			Store:    store,
			Source:   localFileSource{},
			Machine:  machine,
			StateDir: stateDir(),
		},
		Updater: localUpdater{},
	}, nil
}

// readLocalState answers `agent.exe status` from the machine itself. It
// deliberately does not sync: an operator checking on a machine must not
// rewrite its registry or restart the employee's AI tools as a side effect.
func readLocalState() (localReport, error) {
	machine, err := newLocalMachine()
	if err != nil {
		return localReport{}, err
	}
	return localState(machine, version), nil
}

// printLocalState reports only what the machine itself can answer. Anything
// that lives in the store -- who this machine is assigned to, when it last
// synced -- is deliberately absent rather than shown as empty.
func printLocalState(r localReport) {
	fmt.Printf("config=%s\n", config.Source(builtIn()))
	fmt.Printf("machine=%s\n", r.Machine)
	fmt.Printf("local users: %s\n", strings.Join(r.LocalUsers, ", "))

	if len(r.UsersWithCreds) == 0 {
		fmt.Println("AI credentials in place for: (nobody)")
	} else {
		fmt.Printf("AI credentials in place for: %s\n", strings.Join(r.UsersWithCreds, ", "))
	}

	fmt.Printf("block=%v domains=%d applocker=%s\n",
		r.BlockEnabled, len(r.BlockedDomains), r.AppLockerMode)
	for _, d := range r.BlockedDomains {
		fmt.Println("  -", d)
	}

	if len(r.Errors) > 0 {
		fmt.Printf("errors (%d):\n", len(r.Errors))
		for _, e := range r.Errors {
			fmt.Println("  -", e)
		}
	}

	fmt.Println()
	fmt.Println("this is a local read only; it does not sync.")
	fmt.Println("for the assigned employee and last sync time, see 'admin.exe status',")
	fmt.Println("or run 'agent.exe sync' to perform a sync now.")
}

func runOnce() (model.Status, error) {
	s, err := newSyncer()
	if err != nil {
		return model.Status{}, err
	}
	return s.RunOnce()
}

// runService runs the long-lived loop, either under the service control
// manager or in the foreground when started manually for debugging.
func runService() {
	dir := stateDir()
	_ = os.MkdirAll(dir, 0o700)
	if f, err := openLog(dir); err == nil {
		log.SetOutput(f)
		defer f.Close()
	}

	worker := func(stop <-chan struct{}, wake <-chan struct{}) {
		s, err := newSyncer()
		if err != nil {
			log.Printf("startup failed: %v", err)
			return
		}
		loop(s, stop, wake)
	}

	if winsvc.IsWindowsService() {
		if err := winsvc.Run(serviceName, winsvc.Hooks{Run: worker}); err != nil {
			log.Printf("service exited: %v", err)
		}
		return
	}
	log.Printf("running in the foreground (not started by the service manager)")
	worker(make(chan struct{}), make(chan struct{}))
}

// loop drives the sync cycle from three independent triggers.
//
// The ticker alone is not enough: it runs on the monotonic clock, which stops
// while the machine sleeps. A desktop that suspends for four hours would
// otherwise keep an outdated policy for the remainder of its interval, right
// when the employee has just resumed work. The wake channel covers resume
// events, and the wall-clock check covers a resume whose event never arrived.
func loop(s *agentcore.Syncer, stop <-chan struct{}, wake <-chan struct{}) {
	interval := time.Duration(s.FallbackInterval) * time.Minute
	if interval <= 0 {
		interval = time.Duration(model.DefaultSyncInterval) * time.Minute
	}

	var lastRun time.Time
	sync := func(reason string) {
		if !lastRun.IsZero() && time.Since(lastRun) < minSyncGap {
			log.Printf("skipping %s sync: another one ran %v ago", reason, time.Since(lastRun).Round(time.Second))
			return
		}
		lastRun = time.Now()

		st, err := s.RunOnce()
		if err != nil {
			log.Printf("%s sync failed: %v", reason, err)
			return
		}
		log.Printf("%s sync ok: policyEtag=%s credsEtag=%s interval=%dm errors=%d",
			reason, orDash(st.PolicyETag), orDash(st.CredsETag), st.SyncIntervalMinutes, len(st.Errors))
		for _, e := range st.Errors {
			log.Printf("  - %s", e)
		}
		// Push the recent log to OSS so the admin can read it without reaching
		// the machine. Best effort: a failed upload (e.g. the RAM policy has no
		// _logs/ write yet) is logged, not fatal.
		if tail := readLogTail(stateDir()); tail != nil {
			if err := s.UploadLog(tail); err != nil {
				log.Printf("log upload failed: %v", err)
			}
		}
		// Honour a remotely changed interval from the next cycle onwards.
		interval = s.NextInterval(st)
	}

	// Trigger 1: the service just started, so do not wait out a whole interval.
	sync("startup")

	// A short tick keeps the wall-clock check responsive without syncing often.
	const checkEvery = time.Minute
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			log.Printf("stopping")
			return

		case <-wake:
			// Trigger 2: the machine resumed from sleep.
			sync("wake-up")

		case <-ticker.C:
			// Trigger 3: the wall clock says we are overdue. This also catches
			// a resume whose power event was never delivered.
			if s.DueForSync(interval) {
				sync("scheduled")
			} else if err := s.Heartbeat(); err != nil {
				// Between full syncs, just refresh "last seen" so the admin can
				// tell the machine is alive without waiting a whole interval.
				log.Printf("heartbeat failed: %v", err)
			}
		}
	}
}
