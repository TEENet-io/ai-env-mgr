//go:build !windows

package main

func codexRunningFor(string) (bool, error) { return false, nil }
