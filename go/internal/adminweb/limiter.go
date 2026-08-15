package adminweb

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	errTooManySessions = errors.New("too many active sessions")
	errRateLimited     = errors.New("too many sign-in attempts; wait a moment and try again")
)

// loginLimiter throttles sign-in attempts per client address.
//
// The console is reachable from the internet, so the login handler is the one
// endpoint anyone can call unauthenticated. Every attempt costs an OSS
// round-trip to verify the credentials, so without a limit a stranger can burn
// the account's API quota and fill the logs at no cost to themselves.
type loginLimiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	window  time.Duration
	max     int
	nowFunc func() time.Time // overridable in tests
}

func newLoginLimiter(window time.Duration, max int) *loginLimiter {
	return &loginLimiter{
		hits:    make(map[string][]time.Time),
		window:  window,
		max:     max,
		nowFunc: time.Now,
	}
}

// allow records an attempt from key and reports whether it may proceed.
func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowFunc()
	cutoff := now.Add(-l.window)

	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)

	// Drop addresses that have gone quiet, so the map does not grow forever
	// under a spray of one attempt each from many addresses.
	if len(l.hits) > 4096 {
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// clientKey identifies the caller for rate limiting.
//
// It deliberately uses the peer address and ignores X-Forwarded-For: that
// header is attacker-controlled unless a trusted proxy overwrites it, and
// trusting it here would let anyone bypass the limit by varying the header.
// Behind a reverse proxy this makes the limit apply to the proxy as a whole,
// which is the safe direction to be wrong in.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
