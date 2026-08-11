//go:build !windows

// Package winsvc wraps Windows service registration and the service main loop.
//
// This stub lets the rest of the module build, vet and test on a Linux
// development machine; every entry point reports that it needs Windows.
package winsvc

import "fmt"

// Hooks carries the callbacks the service body needs.
type Hooks struct {
	Run     func(stop <-chan struct{}, wake <-chan struct{})
	OnEvent func(evt Event)
}

var errWindowsOnly = fmt.Errorf("service control is only supported on Windows")

// Install registers the current executable as an auto-start service.
func Install(name, displayName, desc string) error { return errWindowsOnly }

// Uninstall removes the service.
func Uninstall(name string) error { return errWindowsOnly }

// Start starts an installed service.
func Start(name string) error { return errWindowsOnly }

// Stop stops an installed service.
func Stop(name string) error { return errWindowsOnly }

// Run blocks running the service main loop.
func Run(name string, hooks Hooks) error { return errWindowsOnly }

// IsWindowsService reports whether the process was started by the service
// control manager.
func IsWindowsService() bool { return false }

// IsInstalled is the non-Windows stand-in.
func IsInstalled(name string) (bool, error) { return false, errWindowsOnly }

// IsRunning is the non-Windows stand-in.
func IsRunning(name string) (bool, error) { return false, errWindowsOnly }
