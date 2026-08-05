//go:build !windows

package status

// AppLockerMode is a Windows concept; elsewhere the mode is simply unknown.
// The stub keeps the package testable on a Linux development machine.
func AppLockerMode() string { return "Unknown" }
