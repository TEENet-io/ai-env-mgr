package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CheckConsoleURL accepts only a bare origin, https://host[:port]. The
// device token and employees' credential bundles travel over this
// connection, so plain http is refused unless the host is this machine's
// loopback, where the traffic never leaves the box (the end-to-end rig).
func CheckConsoleURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("console address %q: %w", raw, err)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("console address %q: no host", raw)
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(raw, "?") || strings.HasSuffix(raw, "#") {
		return fmt.Errorf("console address %q: must be scheme://host[:port] and nothing else", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("console address %q: must use https", raw)
	default:
		return fmt.Errorf("console address %q: must use https", raw)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
