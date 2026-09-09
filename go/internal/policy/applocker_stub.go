//go:build !windows

package policy

import "errors"

// ErrAppLockerNotDeployed mirrors the Windows sentinel so callers compile
// and switch on it everywhere.
var ErrAppLockerNotDeployed = errors.New("AppLocker is not deployed on this machine")

// ApplyAppLocker is a no-op off Windows.
//
// Unlike Apply, which returns an error because calling it off Windows is a
// programming mistake, this one is invoked unconditionally on every sync
// cycle; erroring would turn every cycle of a non-Windows build into a
// status full of noise for a machine that has no AppLocker to manage.
func ApplyAppLocker(paths []string) error { return nil }

// LocalAppLockerPaths is the non-Windows stand-in for reading the local
// AppLocker policy back out. There is nothing to read here, and unlike a
// real failed read this is not worth reporting as an error: it mirrors
// Current() reporting nothing applied off Windows.
func LocalAppLockerPaths() ([]string, error) { return nil, nil }
