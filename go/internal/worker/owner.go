package worker

import (
	"fmt"
	"os"
)

// defaultOwner names this process in leases and attempt rows: host and pid,
// which is what somebody reading an incident needs in order to find the log.
func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}
