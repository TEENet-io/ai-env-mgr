package adminweb

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
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

	// Bucket and Endpoint are the deployment's fixed OSS location, baked into
	// the binary the same way the CLI bakes them (cmd/admin/credentials.go),
	// so sign-in only asks for the AccessKey.
	//
	// When set they are also ENFORCED, not merely pre-filled: the handler uses
	// these and ignores whatever the form carried, so the console cannot be
	// pointed at another bucket or endpoint by editing the page.
	Bucket   string
	Endpoint string

	// BehindProxy says a reverse proxy terminates TLS in front of the console.
	//
	// It permits serving plaintext on a PRIVATE address -- the case where the
	// proxy runs in a container and cannot reach the host over loopback -- and
	// nothing more: a public address still demands a certificate, because there
	// the plaintext really would cross the internet. It also restores the two
	// things the proxy would otherwise hide: the Secure cookie flag and HSTS,
	// which depend on the browser's scheme rather than this hop's.
	BehindProxy bool

	// DataEndpoint moves bytes over a different route from the one links are
	// signed with.
	//
	// Set it to the region's internal OSS endpoint when the console runs on a
	// host inside that region. Publishing a 509 MB installer over this host's
	// public egress measured 163 KB/s -- close to an hour -- because it counts
	// against the instance's internet bandwidth; the internal endpoint does not.
	// Presigned links keep using Endpoint, since an internal host is
	// unreachable from wherever such a link is actually opened.
	//
	// Empty means "use Endpoint for both", which is right everywhere else.
	DataEndpoint string

	// GatewayURL is the LiteLLM gateway's base address, e.g.
	// "https://litellm.teenet.app". Empty disables the model-gateway page
	// entirely rather than showing controls that cannot work.
	GatewayURL string

	// GatewayAdminKey authenticates this console to the gateway's management
	// API. It is injected from the environment at startup and lives only in
	// this process's memory, like every other credential here: never written
	// to disk, logged, or placed in a cookie.
	//
	// It is a proxy_admin key rather than the gateway's master key, so it can
	// be revoked on its own without rotating the gateway's own secret.
	GatewayAdminKey string

	// ECDRegion is where the cloud desktops live. Not a secret, and no key
	// belongs here: the lookup uses the credentials the administrator signed
	// in with, so nothing is stored and nothing ships inside the binary.
	// Empty disables the lookup, leaving quiet machines reading as offline.
	ECDRegion string
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

	// pending holds in-flight employee sign-ins, keyed by the session's CSRF
	// token so one console session cannot finish another's flow.
	pendingMu sync.Mutex
	pending   map[string]*pendingLogin

	// jobs holds the one publish that may be in flight; see job.go.
	jobs jobRunner
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
	// classify lets a row ask for its own state without the template
	// re-deriving the rules that fleet.go already owns.
	tpl, err := template.New("").Funcs(template.FuncMap{
		"classify": stateSeverity,
		"state":    stateLabel,
		"age":      humanAge,
		"noscan":   noEmailScan,
		"codex":    codexNote,
		"codexsev": codexNoteSeverity,
		"ctxsize":  contextWindowLabel,
		"usagepct": usagePercent,
		"usagesev": usageSeverity,
		"money":    func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
		"has": func(list []string, v string) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		},
	}).ParseFS(assetFS, "assets/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		opts:     opts,
		sessions: newSessionStore(opts.IdleTTL, opts.AbsTTL),
		limiter:  newLoginLimiter(time.Minute, 10),
		tpl:      tpl,
		pending:  make(map[string]*pendingLogin),
		dialOSS: func(cfg config.Config) (store, error) {
			data := cfg.Endpoint
			if opts.DataEndpoint != "" {
				data = opts.DataEndpoint
			}
			return ossclient.NewSplit(data, cfg.Endpoint, cfg.Bucket, cfg.AccessKeyID, cfg.AccessKeySecret)
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
	mux.HandleFunc("/users", s.requireSession(s.handleUsers))
	mux.HandleFunc("/users/detail", s.requireSession(s.handleUserDetail))
	mux.HandleFunc("/sites", s.requireSession(s.handleSites))
	mux.HandleFunc("/settings", s.requireSession(s.handleSettings))
	mux.HandleFunc("/log", s.requireSession(s.handleLog))
	mux.HandleFunc("/files", s.requireSession(s.handleFiles))
	mux.HandleFunc("/rollout", s.requireSession(s.handleRollout))
	mux.HandleFunc("/employee-login", s.requireSession(s.handleEmployeeLogin))
	mux.HandleFunc("/gateway", s.requireSession(s.handleGateway))

	// Every state-changing route is POST + CSRF + redirect (see requirePost).
	mux.HandleFunc("/users/onboard", s.requirePostBack(backToAccount, s.actionAccountOnboard))
	mux.HandleFunc("/users/reopen", s.requirePostBack(backToAccount, s.actionAccountReopen))
	mux.HandleFunc("/users/offboard", s.requirePostBack(backToAccount, s.actionAccountOffboard))
	mux.HandleFunc("/users/quota", s.requirePostBack(backToAccount, s.actionAccountQuota))
	mux.HandleFunc("/users/models", s.requirePostBack(backToAccount, s.actionAccountModels))
	mux.HandleFunc("/users/reissue", s.requirePostBack(backToAccount, s.actionAccountReissue))
	mux.HandleFunc("/users/profile", s.requirePostBack(backToAccount, s.actionAccountProfile))
	mux.HandleFunc("/machines/bind", s.requirePost("/machines", s.actionMachineBind))
	mux.HandleFunc("/machines/unbind", s.requirePost("/machines", s.actionMachineUnbind))
	mux.HandleFunc("/sites/mutate", s.requirePost("/sites", s.actionSites))
	mux.HandleFunc("/sites/enabled", s.requirePost("/sites", s.actionBlockEnabled))
	mux.HandleFunc("/sites/applocker", s.requirePost("/sites", s.actionAppLocker))
	mux.HandleFunc("/sites/applocker-mode", s.requirePost("/sites", s.actionAppLockerMode))
	mux.HandleFunc("/settings/interval", s.requirePost("/settings", s.actionSyncInterval))
	mux.HandleFunc("/settings/collect", s.requirePost("/settings", s.actionCollect))
	mux.HandleFunc("/settings/quota-defaults", s.requirePost("/settings", s.actionQuotaDefaults))

	// Fleet-wide and irreversible actions. Each additionally demands the exact
	// version or hostname typed back (see confirmMatches) and writes an audit
	// line, because these are the ones that make every machine run a binary or
	// that cannot be undone.
	mux.HandleFunc("/agent/publish", s.requirePost("/rollout", s.actionAgentPublish))
	mux.HandleFunc("/agent/cancel", s.requirePost("/rollout", s.actionAgentCancel))
	mux.HandleFunc("/codex/publish", s.requirePost("/rollout", s.actionCodexPublish))
	mux.HandleFunc("/codex/cancel", s.requirePost("/rollout", s.actionCodexCancel))
	mux.HandleFunc("/machines/forget", s.requirePost("/machines", s.actionMachineForget))
	mux.HandleFunc("/files/put", s.requirePost("/files", s.actionFilePut))
	mux.HandleFunc("/files/rm", s.requirePost("/files", s.actionFileRemove))
	mux.HandleFunc("/employee-login/start", s.requirePost("/employee-login", s.actionEmployeeLoginStart))
	mux.HandleFunc("/employee-login/finish", s.requirePost("/employee-login", s.actionEmployeeLoginFinish))
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

// publishDrainTimeout bounds how long shutdown waits for a publish to finish.
//
// Long enough for the slow half of the job -- several hundred megabytes up to
// OSS -- and short enough that a wedged job cannot hold the service down
// indefinitely. systemd's TimeoutStopSec must exceed it, or systemd sends
// SIGKILL first and the wait buys nothing.
const publishDrainTimeout = 20 * time.Minute

// ListenAndServe runs the console until it fails, finishing any publish that
// is in flight before it exits.
func (s *Server) ListenAndServe() error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

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
	serveErr := make(chan error, 1)
	go func() {
		if s.opts.CertFile != "" {
			serveErr <- srv.ListenAndServeTLS(s.opts.CertFile, s.opts.KeyFile)
			return
		}
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return err
	case sig := <-stop:
		// Stop taking requests first, so nothing new starts while draining.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()

		if j := s.jobs.snapshot(); j != nil && j.Running() {
			log.Printf("%s received; finishing the %s %s publish before exiting (up to %s)",
				sig, j.Kind, j.Version, publishDrainTimeout)
			if s.jobs.wait(publishDrainTimeout) {
				log.Printf("publish finished; exiting")
			} else {
				// Said out loud because the alternative is an operator who
				// believes a publish landed when it did not.
				log.Printf("publish did NOT finish within %s; exiting anyway. "+
					"The policy was not updated -- publish it again.", publishDrainTimeout)
			}
		}
		return nil
	}
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
