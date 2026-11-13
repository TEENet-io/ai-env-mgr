//go:build !windows

package managedskills

import "errors"

func Protect(string) error             { return errors.New("machine-level Codex skill ACLs require Windows") }
func ProtectPrivate(path string) error { return Protect(path) }
func rejectReparse(string) error       { return nil }
