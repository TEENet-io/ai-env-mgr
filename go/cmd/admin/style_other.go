//go:build !windows

package main

// enableVT is a no-op off Windows: Unix terminals interpret ANSI natively.
func enableVT() bool { return true }
