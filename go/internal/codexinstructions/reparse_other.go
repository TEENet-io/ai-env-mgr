//go:build !windows

package codexinstructions

func rejectReparse(string) error { return nil }
