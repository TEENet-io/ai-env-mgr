// Command agent runs on each cloud desktop as a Windows service. It pulls
// policy, credentials and software tasks from the Admin console, applies them
// locally, and reports back what it did. It never carries bucket credentials.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/policy"
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
	// The AppLocker policy handed to Set-AppLockerPolicy is staged here, not
	// in os.TempDir() -- which for a service running as LocalSystem is
	// C:\Windows\Temp, a directory standard users can write to. See
	// policy.SetAppLockerStagingDir.
	policy.SetAppLockerStagingDir(stateDir())

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
	fmt.Printf("config=console (%s)\n", builtIn().ConsoleURL)
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
	// No count of published allow paths here: that is intent, not machine
	// truth. `agent.exe status` reads the machine's own AppLocker policy
	// instead (see printLocalState / formatAppLockerAllow).
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

// formatAppLockerAllow renders localReport.AppLockerAllowPaths for
// `agent.exe status` output. appLockerAllowUnknown (the local AppLocker
// policy could not be read) must print visibly differently from a confirmed
// empty list -- "?" rather than "0" -- so an operator reading this line does
// not mistake "we don't know" for "confirmed nothing applied". The reason
// for the unknown state lands separately in localReport.Errors.
func formatAppLockerAllow(n int) string {
	if n == appLockerAllowUnknown {
		return "?"
	}
	return strconv.Itoa(n)
}

// newSyncer wires the local applier to the console-backed source. The agent
// deliberately has no OSS client or bucket credentials.
func newSyncer() (*agentcore.Syncer, error) {
	cfg, _, err := config.ResolveAgent(builtIn(), config.DefaultAgentPath())
	if err != nil {
		return nil, err
	}
	machine, err := newLocalMachine()
	if err != nil {
		return nil, err
	}
	s := &agentcore.Syncer{
		Applier:          localApplier{},
		Machine:          machine,
		Version:          version,
		StateDir:         stateDir(),
		FallbackInterval: cfg.IntervalMinutes,
		Collector: &agentcore.Collector{
			Store:    nil,
			Source:   localFileSource{},
			Machine:  machine,
			StateDir: stateDir(),
		},
		Updater:      localUpdater{},
		Codex:        newCodexInstaller(),
		Applications: newApplicationInstaller(),
	}
	// Application downloads can be hundreds of megabytes and therefore outlive
	// a normal sync cycle. Flush a progress line before each long phase so the
	// local log and the Admin log tail show that the Agent is working instead
	// of looking frozen until the installer finishes.
	s.ApplicationProgress = func(st model.ApplicationStatus) {
		log.Printf("application %s@%s task=%s state=%s", st.AppID, st.DesiredVersion, st.TaskID, st.State)
		if tail := readLogTail(stateDir()); tail != nil {
			if err := s.UploadLog(tail); err != nil {
				log.Printf("application progress log upload failed: %v", err)
			}
		}
	}
	// The console, when this build knows one: instructions and reports go
	// there, and session files are uploaded through links it signs.
	consoleTarget = cfg.ConsoleURL
	if consoleTarget != "" {
		if err := config.CheckConsoleURL(consoleTarget); err != nil {
			return nil, err
		}
	}
	if api := connectConsole(consoleTarget, machine.Name(), stateDir()); api != nil {
		adoptConsole(s, api)
	}
	return s, nil
}

// consoleTarget is the console this build talks to.
// The service loop uses it to keep enrolling when the start-up attempt
// did not get a token.
var consoleTarget string

// adoptConsole points the syncer at the console for instructions, reports
// and session uploads.
func adoptConsole(s *agentcore.Syncer, api *agentcore.APISource) {
	s.SetSource(api)
	if c, ok := s.Collector.(*agentcore.Collector); ok {
		c.Store = api
	}
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
	fmt.Printf("config=console (%s)\n", builtIn().ConsoleURL)
	if consoleURL != "" {
		fmt.Printf("console=%s\n", consoleURL)
	} else {
		fmt.Println("console=none (not configured)")
	}
	fmt.Printf("machine=%s\n", r.Machine)
	fmt.Printf("local users: %s\n", strings.Join(r.LocalUsers, ", "))

	if len(r.UsersWithCreds) == 0 {
		fmt.Println("AI credentials in place for: (nobody)")
	} else {
		fmt.Printf("AI credentials in place for: %s\n", strings.Join(r.UsersWithCreds, ", "))
	}

	fmt.Printf("block=%v domains=%d applocker=%s applocker_allow=%s\n",
		r.BlockEnabled, len(r.BlockedDomains), r.AppLockerMode, formatAppLockerAllow(r.AppLockerAllowPaths))
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

	// Build the syncer once and share it between the worker loop and the
	// power/stop event handler: OnEvent must report on the very same status the
	// loop maintains, so they cannot each own a private copy.
	s, err := newSyncer()
	if err != nil {
		log.Printf("startup failed: %v", err)
		return
	}

	hooks := winsvc.Hooks{
		Run: func(stop <-chan struct{}, wake <-chan struct{}) { loop(s, stop, wake) },
		OnEvent: func(evt winsvc.Event) {
			// Report the transition so the admin sees "sleeping"/"stopped"
			// instead of an unexplained silence. Bounded inside ReportEvent.
			log.Printf("lifecycle event: %s -- reporting before going quiet", evt)
			s.ReportEvent(evt.String())
		},
	}

	if winsvc.IsWindowsService() {
		if err := winsvc.Run(serviceName, hooks); err != nil {
			log.Printf("service exited: %v", err)
		}
		return
	}
	log.Printf("running in the foreground (not started by the service manager)")
	hooks.Run(make(chan struct{}), make(chan struct{}))
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

	// Trigger 4, in the console mode: the console says something changed.
	// One long poll after another, a nudge here when one returns "changed";
	// the ticker below is then only the wall-clock fallback.
	notify := make(chan struct{}, 1)
	nudge := func() {
		select {
		case notify <- struct{}{}:
		default: // one is already pending
		}
	}

	var lastRun time.Time
	type syncResult struct {
		reason string
		status model.Status
		err    error
	}
	syncDone := make(chan syncResult, 1)
	syncing := false
	pendingSync := false

	startSync := func(reason string) {
		if !s.Ready() {
			log.Printf("%s sync waiting for console enrolment", reason)
			return
		}
		if syncing {
			pendingSync = true
			log.Printf("skipping %s sync: another sync is still running", reason)
			return
		}
		if !lastRun.IsZero() && time.Since(lastRun) < minSyncGap {
			since := time.Since(lastRun).Round(time.Second)
			if reason == "console changed" {
				// The console's change must not be lost to the debounce:
				// come back when the gap has passed.
				log.Printf("deferring %s sync: another one ran %v ago", reason, since)
				time.AfterFunc(minSyncGap-time.Since(lastRun), nudge)
				return
			}
			log.Printf("skipping %s sync: another one ran %v ago", reason, since)
			return
		}
		lastRun = time.Now()
		syncing = true
		// RunOnce can download and install a large application. Keep it off the
		// service/event loop so heartbeats, long-poll configuration changes and
		// stop handling continue while that work is in progress.
		go func() {
			st, err := s.RunOnce()
			syncDone <- syncResult{reason: reason, status: st, err: err}
		}()
	}

	finishSync := func(result syncResult) {
		syncing = false
		if pendingSync {
			pendingSync = false
			nudge()
		}
		if result.err != nil {
			log.Printf("%s sync failed: %v", result.reason, result.err)
			return
		}
		st := result.status
		// Errors and warnings are counted separately: "errors=1" for a machine
		// merely waiting to be signed in for reads as a fault when nothing is
		// wrong, and a log that says that routinely is one nobody trusts.
		log.Printf("%s sync ok: policyEtag=%s credsEtag=%s interval=%dm errors=%d warnings=%d",
			result.reason, orDash(st.PolicyETag), orDash(st.CredsETag), st.SyncIntervalMinutes,
			len(st.Errors), len(st.Warnings))
		for _, e := range st.Errors {
			log.Printf("  ! %s", e)
		}
		for _, w := range st.Warnings {
			log.Printf("  - %s", w)
		}
		for _, app := range st.Apps {
			log.Printf("  application %s@%s task=%s state=%s error=%s",
				app.AppID, app.DesiredVersion, app.TaskID, app.State, orDash(app.LastError))
		}
		// Push the recent log through the console's signed upload URL. Best
		// effort: a failed upload is logged, not fatal.
		if tail := readLogTail(stateDir()); tail != nil {
			if err := s.UploadLog(tail); err != nil {
				log.Printf("log upload failed: %v", err)
			}
		}
		// Honour a remotely changed interval from the next cycle onwards.
		interval = s.NextInterval(st)
	}

	// Trigger 1: the service just started, so do not wait out a whole interval.
	// Application jobs have their own lease-aware worker. An installer may run
	// for 30 minutes without blocking policy sync, heartbeat, or Admin control.
	go applicationTaskLoop(s, stop)
	startSync("startup")

	hostname := s.Machine.Name()
	startWaiter := func(api *agentcore.APISource) {
		go consoleWaiter(api, stop, notify, func() (string, error) {
			return enrolConsole(api.Client.BaseURL, hostname, stateDir())
		}, defaultWaiterOptions())
	}
	adopted := make(chan *agentcore.APISource, 1)
	if api, ok := s.Source.(*agentcore.APISource); ok {
		startWaiter(api)
	} else if consoleTarget != "" {
		// No token yet: keep asking, and switch over when one arrives.
		go enrolUntilDone(consoleTarget, hostname, stateDir(), stop, adopted, defaultWaiterOptions())
	}

	// A short tick keeps the wall-clock check responsive without syncing often.
	const checkEvery = time.Minute
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()

	for {
		select {
		case result := <-syncDone:
			finishSync(result)

		case <-stop:
			log.Printf("stopping")
			return

		case <-notify:
			startSync("console changed")

		case api := <-adopted:
			// Enrolled after start-up: from here on the console is the source.
			adoptConsole(s, api)
			startWaiter(api)
			startSync("console enrolled")

		case <-wake:
			// Trigger 2: the machine resumed from sleep.
			startSync("wake-up")

		case <-ticker.C:
			// Trigger 3: the wall clock says we are overdue. This also catches
			// a resume whose power event was never delivered.
			if s.DueForSync(interval) {
				startSync("scheduled")
			} else if changed, what := s.ChangedSinceLastSync(); changed {
				// The console changed something for this machine; do not
				// make it wait out the interval.
				startSync(what + " changed")
			} else if err := s.Heartbeat(); err != nil {
				// Between full syncs, just refresh "last seen" so the admin can
				// tell the machine is alive without waiting a whole interval.
				log.Printf("heartbeat failed: %v", err)
			}
		}
	}
}
