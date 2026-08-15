package adminweb

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

//go:embed assets/*
var assetFS embed.FS

// Options configures the console. Zero values are filled in by New.
type Options struct {
	Listen   string        // host:port
	CertFile string        // TLS certificate; required unless the bind is loopback
	KeyFile  string        // TLS key
	IdleTTL  time.Duration // sign out after this much inactivity
	AbsTTL   time.Duration // sign out this long after signing in, active or not

	// BehindProxy says a reverse proxy terminates TLS in front of the console.
	//
	// It permits serving plaintext on a PRIVATE address -- the case where the
	// proxy runs in a container and cannot reach the host over loopback -- and
	// nothing more: a public address still demands a certificate, because there
	// the plaintext really would cross the internet. It also restores the two
	// things the proxy would otherwise hide: the Secure cookie flag and HSTS,
	// which depend on the browser's scheme rather than this hop's.
	BehindProxy bool
}

// store is what the console needs from OSS: everything admincore.Manager uses,
// plus an explicit credential check.
//
// Verify is part of the interface rather than an optional type assertion
// because sign-in has nothing else to check against. CurrentPolicy cannot
// serve: it deliberately swallows errors and answers with a default policy so
// a fresh bucket works, which would let anyone in with any credentials.
type store interface {
	admincore.Store
	Verify() error
}

// Server is the administrator console.
type Server struct {
	opts     Options
	sessions *sessionStore
	limiter  *loginLimiter
	tpl      *template.Template
	// dialOSS is the seam tests use to avoid talking to a real bucket.
	dialOSS func(cfg config.Config) (store, error)
}

// New validates the options and builds the server.
func New(opts Options) (*Server, error) {
	if opts.Listen == "" {
		return nil, fmt.Errorf("a listen address is required")
	}
	if opts.IdleTTL == 0 {
		opts.IdleTTL = 30 * time.Minute
	}
	if opts.AbsTTL == 0 {
		opts.AbsTTL = 12 * time.Hour
	}
	// Credentials that can push a binary every machine executes must not cross
	// the network in the clear. Loopback is exempt because nothing leaves the
	// host, and a private address is exempt only when a proxy is declared to be
	// terminating TLS in front. Anything else must present a certificate.
	if opts.CertFile == "" || opts.KeyFile == "" {
		reach, err := classifyListen(opts.Listen)
		if err != nil {
			return nil, err
		}
		switch {
		case reach == reachLoopback:
		case reach == reachPrivate && opts.BehindProxy:
		default:
			hint := "so pass --cert and --key, or bind to 127.0.0.1 and reach it through a tunnel"
			if reach == reachPrivate {
				hint = "so pass --cert and --key, or add --behind-proxy if a reverse proxy terminates TLS in front of it"
			}
			return nil, fmt.Errorf(
				"refusing to serve %s without TLS: sign-in posts OSS credentials that can push a binary to every machine, %s",
				opts.Listen, hint)
		}
	}
	tpl, err := template.ParseFS(assetFS, "assets/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		opts:     opts,
		sessions: newSessionStore(opts.IdleTTL, opts.AbsTTL),
		limiter:  newLoginLimiter(time.Minute, 10),
		tpl:      tpl,
		dialOSS: func(cfg config.Config) (store, error) {
			return ossclient.New(cfg.Endpoint, cfg.Bucket, cfg.AccessKeyID, cfg.AccessKeySecret)
		},
	}, nil
}

// reachability describes who can open a connection to a listen address.
type reachability int

const (
	// reachPublic covers real public addresses and, deliberately, anything
	// unrecognised: a bare port (":8080"), 0.0.0.0, and hostnames all land here
	// so an unclear address is treated as exposed. Guessing the other way would
	// silently serve credentials in the clear, which is what this prevents.
	reachPublic reachability = iota
	reachPrivate
	reachLoopback
)

func classifyListen(listen string) (reachability, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return reachPublic, fmt.Errorf("listen address %q must be host:port: %w", listen, err)
	}
	if host == "localhost" {
		return reachLoopback, nil
	}
	ip := net.ParseIP(host)
	if ip == nil { // empty host, or a name we cannot judge
		return reachPublic, nil
	}
	switch {
	case ip.IsLoopback():
		return reachLoopback, nil
	case ip.IsUnspecified(): // 0.0.0.0 / :: bind every interface, public ones too
		return reachPublic, nil
	case ip.IsPrivate() || ip.IsLinkLocalUnicast():
		return reachPrivate, nil
	default:
		return reachPublic, nil
	}
}

// Handler builds the routes. Exposed so tests can drive the server without
// binding a port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/machines", s.requireSession(s.handleMachines))
	mux.HandleFunc("/policy", s.requireSession(s.handlePolicy))
	// Serve only assets/static, so the templates next to it are never handed
	// out as raw files, and strip the prefix so paths resolve inside it.
	staticFS, err := fs.Sub(assetFS, "assets/static")
	if err != nil {
		// Only reachable if the embedded tree is missing, which is a build-time
		// mistake rather than something to handle at runtime.
		panic("adminweb: embedded assets/static missing: " + err.Error())
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	return s.secureHeaders(mux)
}

// ListenAndServe runs the console until it fails.
func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:    s.opts.Listen,
		Handler: s.Handler(),
		// A sign-in posts a form and the handler verifies it against OSS, so
		// the read side is short while the write side allows for that check.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	if s.opts.CertFile != "" {
		return srv.ListenAndServeTLS(s.opts.CertFile, s.opts.KeyFile)
	}
	return srv.ListenAndServe()
}

// secureHeaders applies defence-in-depth headers to every response.
//
// The CSP is deliberately strict and the assets are self-contained: no CDN, no
// inline script. A cross-site script on this origin would be able to drive the
// console with the operator's session.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// Credentials pass through the sign-in form; keep them out of caches.
		h.Set("Cache-Control", "no-store")
		if s.browserUsesTLS() {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// browserUsesTLS reports whether the browser reached us over HTTPS, which is
// what the Secure cookie flag and HSTS must key off. Serving plaintext to a
// proxy that fronts us with TLS still counts: the browser sees HTTPS, so
// marking the cookie Secure is both correct and necessary -- without it the
// cookie would also ride along any plaintext request to the same host.
func (s *Server) browserUsesTLS() bool { return s.opts.CertFile != "" || s.opts.BehindProxy }
