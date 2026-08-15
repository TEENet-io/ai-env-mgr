package adminweb

import (
	"errors"
	"net"
	"net/http"
	"strings"
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
// Direct serving uses the peer address and ignores X-Real-IP, because that
// header is attacker-controlled and trusting it would let anyone bypass the
// limit by varying it.
//
// Behind a declared proxy the peer address is always the proxy, which would
// collapse the limit into a single global bucket and let one noisy address
// lock everyone out. There X-Real-IP is trustworthy for a specific reason:
// --behind-proxy only serves a private address, so the proxy is the only thing
// that can reach the socket, and it overwrites the header on every request.
func (s *Server) clientKey(r *http.Request) string {
	if s.opts.BehindProxy {
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
