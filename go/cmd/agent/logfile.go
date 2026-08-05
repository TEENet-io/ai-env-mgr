package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// maxLogBytes is when the log gets rolled. Small enough that a year of
	// one-minute syncs cannot fill a cloud desktop's disk, large enough to
	// still hold days of history for troubleshooting.
	maxLogBytes = 4 << 20

	// One previous file is kept. Two generations covers "what happened
	// overnight" without turning the directory into an archive nobody prunes.
	logBackupSuffix = ".1"
)

// openLog opens the agent's log, rolling it first if it has grown past
// maxLogBytes.
//
// Rotation matters more than it looks: the sync interval is administrator-
// controlled and can be set as low as one minute, which is roughly 1400 lines
// a day. Appending forever would grow without bound on a machine nobody logs
// into to clean up.
func openLog(dir string) (*os.File, error) {
	path := filepath.Join(dir, "agent.log")

	if info, err := os.Stat(path); err == nil && info.Size() >= maxLogBytes {
		// A failed roll must not stop the agent from running or logging, so
		// the error is reported into the fresh file rather than returned.
		rollErr := os.Rename(path, path+logBackupSuffix)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		if rollErr != nil {
			fmt.Fprintf(f, "could not roll the previous log: %v\n", rollErr)
		}
		return f, nil
	}

	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}
