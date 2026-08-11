//go:build windows

// Package winsvc wraps Windows service registration and the service main loop.
//
// Beyond start/stop it also subscribes to power events: cloud desktops sleep,
// and a timer alone would leave a woken machine on a stale policy until its
// interval elapsed.
package winsvc

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Power event codes delivered through svc.ChangeRequest.EventType when the
// service accepts SERVICE_ACCEPT_POWEREVENT.
const (
	pbtAPMResumeSuspend   = 0x0007 // resumed after user-initiated suspend
	pbtAPMResumeAutomatic = 0x0012 // system woke itself
	pbtAPMSuspend         = 0x0004 // about to sleep
)

// Install registers the current executable as an auto-start service.
func Install(name, displayName, desc string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(name); err == nil {
		s.Close()
		return fmt.Errorf("service %q is already installed", name)
	}
	s, err := m.CreateService(name, exe, mgr.Config{
		DisplayName: displayName,
		Description: desc,
		StartType:   mgr.StartAutomatic,
	}, "run")
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	return nil
}

// Uninstall removes the service.
func Uninstall(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service %q is not installed", name)
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return nil
}

// Start starts an installed service.
func Start(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

// Stop stops an installed service.
func Stop(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	return nil
}

// Hooks carries the callbacks the service body needs.
type Hooks struct {
	// Run is the worker. It should return when stop is closed.
	// Wake receives a signal every time the machine resumes from sleep.
	Run func(stop <-chan struct{}, wake <-chan struct{})

	// OnEvent, if set, is called synchronously by the service control handler
	// when the machine is about to suspend (EventSuspend) or the service is
	// stopping (EventStop), BEFORE the worker is told to stop. It must return
	// quickly -- the OS and the service control manager are both waiting on it
	// -- so the implementation is expected to bound its own work. It exists so
	// the agent can report an orderly transition before it loses the network.
	OnEvent func(evt Event)
}

type handler struct {
	hooks Hooks
}

func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	// Accepting POWEREVENT is what makes resume notifications arrive at all.
	// PRESHUTDOWN is what makes the stop report survive a real machine
	// shutdown: a plain SHUTDOWN control arrives so late that the network is
	// already being torn down (the SCM even reports "shutdown in progress"),
	// leaving no window to reach OSS. PRESHUTDOWN fires BEFORE that sequence,
	// with a generous default timeout (~180s), so the agent has time to report
	// "stopped" before anything goes away. A service accepting preshutdown is
	// sent PRESHUTDOWN in place of SHUTDOWN, so both are handled the same.
	const accepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown | svc.AcceptPowerEvent

	changes <- svc.Status{State: svc.StartPending}

	stop := make(chan struct{})
	// Buffered so a resume notification is never lost while the worker is busy.
	wake := make(chan struct{}, 1)

	go h.hooks.Run(stop, wake)
	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
			time.Sleep(100 * time.Millisecond)
			changes <- c.CurrentStatus

		case svc.PowerEvent:
			switch c.EventType {
			case pbtAPMResumeSuspend, pbtAPMResumeAutomatic:
				// Non-blocking: if a wake is already queued, one is enough.
				select {
				case wake <- struct{}{}:
				default:
				}
			case pbtAPMSuspend:
				// About to sleep: report the transition while the network is
				// (briefly) still up, so the admin sees "sleeping" rather than
				// an unexplained silence. Best effort and bounded by OnEvent.
				if h.hooks.OnEvent != nil {
					h.hooks.OnEvent(EventSuspend)
				}
			}

		case svc.Stop, svc.Shutdown, svc.PreShutdown:
			// Tell the SCM we heard it before doing any work, so a slow report
			// cannot make the service look hung.
			changes <- svc.Status{State: svc.StopPending}
			// Report the orderly stop before the worker tears down. For a plain
			// service stop the network is plainly up; for a machine shutdown the
			// PRESHUTDOWN control (see above) is what gets us here early enough
			// for the report to still reach OSS.
			if h.hooks.OnEvent != nil {
				h.hooks.OnEvent(EventStop)
			}
			close(stop)
			return false, 0
		}
	}
	return false, 0
}

// Run blocks running the service main loop.
func Run(name string, hooks Hooks) error {
	return svc.Run(name, &handler{hooks: hooks})
}

// IsWindowsService reports whether the process was started by the service
// control manager, as opposed to being run from a console for debugging.
func IsWindowsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

// IsInstalled reports whether the service is registered.
func IsInstalled(name string) (bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return false, nil
	}
	s.Close()
	return true, nil
}

// IsRunning reports whether the service is currently running.
func IsRunning(name string) (bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return false, nil
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return false, fmt.Errorf("query service: %w", err)
	}
	return st.State == svc.Running, nil
}
