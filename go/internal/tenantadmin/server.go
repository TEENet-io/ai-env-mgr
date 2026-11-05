package tenantadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limits on what a console may send and what is read back.
const (
	maxRequestBody  = 1 << 20
	maxResponseBody = 16 << 20
)

// Server is the HTTP side of tenant-admin. Mount it with the /tenant-admin
// prefix already stripped (nginx does that).
type Server struct {
	Upstream Upstream
	// Config is read on every request, so a reload takes effect at once.
	Config func() Config
	Log    *log.Logger
	// Now is the clock, replaceable in tests.
	Now func() time.Time

	failures failureLimiter
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	rec := entry{IP: clientIP(r), Method: r.Method, Path: r.URL.Path}
	defer func() {
		rec.Millis = s.now().Sub(start).Milliseconds()
		s.logEntry(rec)
	}()

	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		s.healthz(w, r, &rec)
		return
	}
	if s.failures.blocked(rec.IP, start) {
		rec.Result = "429"
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many failed sign-ins; try again later")
		return
	}
	id, ok := s.Config().Authenticate(bearer(r.Header.Get("Authorization")))
	if !ok {
		s.failures.fail(rec.IP, start)
		rec.Result = "401"
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid tenant token")
		return
	}
	rec.Team, rec.TokenID = id.Team, id.TokenID

	if r.Method == http.MethodGet && r.URL.Path == "/whoami" {
		rec.Result = "allowed"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"team": id.Team})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		rec.Result, rec.Reason = "403", "request body too large or unreadable"
		writeError(w, http.StatusForbidden, "tenant_forbidden", rec.Reason)
		return
	}
	c, err := plan(r.Context(), s.Upstream, id.Team, r.Method, r.URL.Path, r.URL.Query(), body)
	var d denied
	var lf lookupFailed
	switch {
	case errors.As(err, &d):
		rec.Result, rec.Reason = "403", d.reason
		writeError(w, http.StatusForbidden, "tenant_forbidden", d.reason)
		return
	case errors.As(err, &lf):
		rec.Result, rec.Reason = "502", lf.Error()
		writeError(w, http.StatusBadGateway, "gateway_error", "could not check the request against the gateway")
		return
	case err != nil:
		rec.Result, rec.Reason = "500", err.Error()
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	status, payload, err := s.Upstream.Do(r.Context(), c.method, c.path, c.body)
	if err != nil {
		rec.Result, rec.Reason = "502", err.Error()
		writeError(w, http.StatusBadGateway, "gateway_error", "the gateway did not answer")
		return
	}
	rec.Result, rec.Upstream = "allowed", status
	if status >= 200 && status < 300 && c.transform != nil {
		if payload, err = c.transform(payload); err != nil {
			rec.Result, rec.Reason = "502", "unreadable gateway response: "+err.Error()
			writeError(w, http.StatusBadGateway, "gateway_error", "unreadable gateway response")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(payload)
}

// healthz reports whether tenant-admin can reach LiteLLM with its key. It
// needs no token and says nothing about tenants.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request, rec *entry) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	status, _, err := s.Upstream.Do(ctx, http.MethodGet, "/key/list?size=1", nil)
	if err != nil || status != http.StatusOK {
		rec.Result = "unhealthy"
		writeError(w, http.StatusServiceUnavailable, "unhealthy", "gateway unreachable or key rejected")
		return
	}
	rec.Result = "healthy"
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"status":"ok"}`)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": kind}})
}

// entry is one access log line. Bodies and tokens are never logged.
type entry struct {
	Time     string `json:"time"`
	IP       string `json:"ip"`
	Team     string `json:"team,omitempty"`
	TokenID  string `json:"token_id,omitempty"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Result   string `json:"result"`
	Reason   string `json:"reason,omitempty"`
	Upstream int    `json:"upstream_status,omitempty"`
	Millis   int64  `json:"ms"`
}

func (s *Server) logEntry(e entry) {
	if s.Log == nil {
		return
	}
	e.Time = s.now().UTC().Format(time.RFC3339)
	line, _ := json.Marshal(e)
	s.Log.Print(string(line))
}

// clientIP is the address nginx saw, restored from Cloudflare's header by
// nginx's real_ip module. The listener is bound to loopback, so only nginx
// can set it.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return host
}

// failureLimiter blocks an address for a while after repeated bad tokens.
// Tokens are 256-bit, so this is about log noise and scanners, not about
// guessing.
type failureLimiter struct {
	mu      sync.Mutex
	entries map[string]*failures
}

type failures struct {
	windowStart  time.Time
	count        int
	blockedUntil time.Time
}

const (
	failureWindow = time.Minute
	failureLimit  = 20
	blockFor      = 10 * time.Minute
)

func (l *failureLimiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.entries[ip]
	return f != nil && now.Before(f.blockedUntil)
}

func (l *failureLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = map[string]*failures{}
	}
	// Forget addresses that have been quiet, so the map stays small.
	for k, f := range l.entries {
		if now.Sub(f.windowStart) > failureWindow && now.After(f.blockedUntil) {
			delete(l.entries, k)
		}
	}
	f := l.entries[ip]
	if f == nil || now.Sub(f.windowStart) > failureWindow {
		f = &failures{windowStart: now}
		l.entries[ip] = f
	}
	f.count++
	if f.count > failureLimit {
		f.blockedUntil = now.Add(blockFor)
	}
}

// HTTPUpstream calls LiteLLM with the administrator key. It builds every
// request afresh: no header the console sent -- Authorization,
// x-litellm-api-key, cookies -- can reach LiteLLM, which would otherwise take
// a key from x-litellm-api-key over the one set here.
type HTTPUpstream struct {
	BaseURL  string
	AdminKey string
	Client   *http.Client
}

func (u HTTPUpstream) Do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(u.BaseURL, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.AdminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("call gateway %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, payload, nil
}
