// Package deviceapi is the agent's channel to the console: enrol, read the
// machine's configuration, wait for it to change, report, fetch what the
// configuration points at. Everything but enrolment is authenticated with
// the device's own token.
package deviceapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Presigner mints the links an agent downloads from and uploads to, so the
// agent never holds bucket credentials.
type Presigner interface {
	SignedURL(key string, ttl time.Duration) (string, error)
	SignedPutURL(key string, ttl time.Duration, contentType string) (string, error)
}

// DataPresigner is optional. OSS clients built with a split public/data
// endpoint implement it so agents in the same VPC receive an internal link;
// test doubles and legacy stores continue using Presigner's public URL.
type DataPresigner interface {
	SignedDataURL(key string, ttl time.Duration) (string, error)
	SignedDataPutURL(key string, ttl time.Duration, contentType string) (string, error)
}

// BucketReader reads one object: the fallback for a credentials bundle that
// predates the bundle table, while the bucket is still written.
type BucketReader interface {
	Get(key string) ([]byte, string, error)
}

// Events is where the API notes what happened. It is never handed a token.
type Events interface {
	Ops(level, eventType, msg string, fields map[string]any)
}

// Server serves /agent/v1/.
type Server struct {
	Store   repo.Store
	Objects Presigner    // nil: artifact and upload links answer 503
	Bucket  BucketReader // nil: no fallback for bundles the table lacks
	Hub     *Hub
	Events  Events
	Now     func() time.Time

	// BehindProxy trusts X-Real-IP for the caller's address, the same rule
	// the console applies to sign-in.
	BehindProxy bool
	// EnrolCIDRs, when set, is the only place enrolments may come from.
	// Read through enrolCIDRs; the settings page replaces it at runtime.
	EnrolCIDRs []net.IPNet
	cidrMu     sync.RWMutex

	// WaitMax bounds a long poll; Recheck is how often it re-reads the
	// configuration without being woken.
	WaitMax time.Duration
	Recheck time.Duration

	// TokenGrace is how long a rotated-out token keeps working.
	TokenGrace time.Duration

	limiter *limiter
	// waits tracks the poll in flight per device, so a newer one can end it.
	waitMu sync.Mutex
	waits  map[string]*poll
}

// poll is one long poll in flight.
type poll struct{ cancel context.CancelFunc }

// takeOver registers p as the device's poll, ending whichever was there.
func (s *Server) takeOver(deviceID string, p *poll) {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	if s.waits == nil {
		s.waits = map[string]*poll{}
	}
	if prev, ok := s.waits[deviceID]; ok && prev != p {
		prev.cancel()
	}
	s.waits[deviceID] = p
}

// release forgets p if it is still the device's poll.
func (s *Server) release(deviceID string, p *poll) {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	if s.waits[deviceID] == p {
		delete(s.waits, deviceID)
	}
}

const (
	// globalLimitKey caps enrolments across all addresses: behind the edge
	// the per-address key is coarse, and a spray from many addresses must
	// not turn into a spray of tokens.
	globalLimitKey = "*"
	globalPerMin   = 60

	maxBody      = 1 << 20
	maxLogTail   = 64 << 10
	artifactTTL  = 15 * time.Minute
	uploadTTL    = 10 * time.Minute
	enrolPerMin  = 10
	headerAgentV = "X-Agent-Version"
)

type ctxKey int

const deviceKey ctxKey = 1

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler routes the API. Mount it at /agent/v1/.
func (s *Server) Handler() http.Handler {
	if s.limiter == nil {
		s.limiter = newLimiter(time.Minute, enrolPerMin)
		s.limiter.maxFor = map[string]int{globalLimitKey: globalPerMin}
	}
	if s.Hub == nil {
		s.Hub = NewHub()
	}
	if s.WaitMax <= 0 {
		s.WaitMax = 25 * time.Second
	}
	if s.Recheck <= 0 {
		s.Recheck = 15 * time.Second
	}
	if s.TokenGrace <= 0 {
		s.TokenGrace = 10 * time.Minute
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/v1/enrol", s.handleEnrol)
	mux.HandleFunc("GET /agent/v1/config", s.authed(s.handleConfig))
	mux.HandleFunc("GET /agent/v1/wait", s.authed(s.handleWait))
	mux.HandleFunc("GET /agent/v1/credentials", s.authed(s.handleCredentials))
	mux.HandleFunc("POST /agent/v1/status", s.authed(s.handleStatus))
	mux.HandleFunc("POST /agent/v1/log", s.authed(s.handleLog))
	mux.HandleFunc("GET /agent/v1/artifact/{product}/{version}", s.authed(s.handleArtifact))
	mux.HandleFunc("GET /agent/v1/application/{appID}/{version}/manifest", s.authed(s.handleApplicationManifest))
	mux.HandleFunc("GET /agent/v1/application/{appID}/{version}", s.authed(s.handleApplication))
	mux.HandleFunc("POST /agent/v1/collect/upload-url", s.authed(s.handleCollectURL))
	mux.HandleFunc("POST /agent/v1/token/rotate", s.authed(s.handleRotate))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, device repo.Device)

// authed checks the bearer token, records the machine as seen, and hands
// the device to the handler. Every failure is the same 401: the caller
// learns nothing about which machines exist.
func (s *Server) authed(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		device, err := s.Store.DeviceTokens().Authenticate(r.Context(), token, s.now())
		if err != nil {
			if !errors.Is(err, repo.ErrNotFound) {
				s.fail(w, r, "authenticate", err)
				return
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Being asked anything is proof the machine is alive.
		if err := s.Store.Devices().MarkSeen(r.Context(), device.ID, strings.TrimSpace(r.Header.Get(headerAgentV)), s.now()); err != nil {
			s.fail(w, r, "mark seen", err)
			return
		}
		next(w, r, device)
	}
}

// fail answers a server-side error without leaking its text to the agent,
// and records it.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if s.Events != nil {
		s.Events.Ops("error", "device_api", what+": "+err.Error(), map[string]any{"path": r.URL.Path})
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) event(level, msg string, device repo.Device, fields map[string]any) {
	if s.Events == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["device_id"] = device.ID
	fields["hostname"] = device.Hostname
	s.Events.Ops(level, "device_api", msg, fields)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// SetEnrolCIDRs replaces the enrolment allow-list; nil or empty means any
// address.
func (s *Server) SetEnrolCIDRs(nets []net.IPNet) {
	s.cidrMu.Lock()
	defer s.cidrMu.Unlock()
	s.EnrolCIDRs = nets
}

func (s *Server) enrolCIDRs() []net.IPNet {
	s.cidrMu.RLock()
	defer s.cidrMu.RUnlock()
	return s.EnrolCIDRs
}

// clientIP is who is calling, by the same rule the console's sign-in uses.
func (s *Server) clientIP(r *http.Request) string {
	if s.BehindProxy {
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

// originIP is the address the enrolment allow-list is checked against.
// Behind the edge, X-Real-IP is the edge's own address; the client's is
// what the edge says in CF-Connecting-IP, which only the edge can set on
// a request that reaches the origin through it. It is used for the
// allow-list and recorded, never for rate limiting.
func (s *Server) originIP(r *http.Request) string {
	if s.BehindProxy {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" && net.ParseIP(cf) != nil {
			return cf
		}
	}
	return s.clientIP(r)
}

// deviceFrom is for handlers written as plain http.HandlerFunc.
func deviceFrom(ctx context.Context) (repo.Device, bool) {
	d, ok := ctx.Value(deviceKey).(repo.Device)
	return d, ok
}

// limiter counts hits per key inside a sliding window.
type limiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	window time.Duration
	max    int
	maxFor map[string]int // keys with their own ceiling
	now    func() time.Time
}

func newLimiter(window time.Duration, max int) *limiter {
	return &limiter{hits: map[string][]time.Time{}, window: window, max: max, now: time.Now}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	limit := l.max
	if m, ok := l.maxFor[key]; ok {
		limit = m
	}
	if len(kept) >= limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	if len(l.hits) > 4096 {
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	return true
}
