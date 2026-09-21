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
	"strings"
	"syscall"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/eventlog"
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

	// LogDir is where the unified-log files go (admin.jsonl, audit.jsonl).
	// Empty means stderr only, which is right for a developer's laptop.
	LogDir string

	// Version is the build this process is, recorded on the start and stop
	// events so a log search can tell which binary produced a run.
	Version string

	// ECDRegion is where the cloud desktops live. Not a secret, and no key
	// belongs here: the lookup uses the credentials the administrator signed
	// in with, so nothing is stored and nothing ships inside the binary.
	// Empty disables the lookup, leaving quiet machines reading as offline.
	ECDRegion string

	// SLSProject is the Simple Log Service project holding the unified log
	// (the "audit" and "ops" logstores). Empty disables the log page the same
	// way an empty GatewayURL disables the gateway page: the nav entry is not
	// drawn and /logs answers 404, rather than offering a page that cannot
	// possibly return anything.
	SLSProject string

	// SLSEndpoint is that project's regional endpoint. Empty means
	// slsclient.DefaultEndpoint. No key belongs here either: the log page
	// reads with the AccessKey the administrator signed in with, so a console
	// that nobody is signed in to holds nothing that could read the logs.
	SLSEndpoint string

	// Database switches the console to the database-backed mode. nil keeps
	// the OSS-backed console exactly as it was.
	Database *DatabaseOptions

	// PublicHost is the name administrators reach the console by. It is what
	// the authenticator app shows, so somebody with three of these on their
	// phone can tell them apart.
	PublicHost string
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

	// jobs holds the one publish that may be in flight; see job.go.
	jobs jobRunner

	// events is the unified log. Never nil after New; every session's
	// Manager shares this one writer, which is safe for concurrent use.
	events *eventlog.Writer

	// dbm is set in the database mode and nil otherwise. Every place that
	// behaves differently between the two checks it, and there are few.
	dbm *dbState
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
		"stamp":    localStamp,
		"noscan":   noEmailScan,
		"codex":    codexNote,
		"codexsev": codexNoteSeverity,
		"alsev":    appLockerSeverity,
		"ctxsize":  contextWindowLabel,
		"usagepct": usagePercent,
		"usagesev": usageSeverity,
		"money":    func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
		"list":     func(xs ...string) []string { return xs },
		"astatus":  artifactSeverity,
		"add":      func(a, b int) int { return a + b },
		"dur":      humanDuration,
		"sub":      func(a, b int) int { return a - b },
		"alabel":   artifactLabel,
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
	host, _ := os.Hostname()
	events, err := eventlog.New(opts.LogDir, "console@"+host)
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts:     opts,
		events:   events,
		sessions: newSessionStore(opts.IdleTTL, opts.AbsTTL),
		limiter:  newLoginLimiter(time.Minute, 10),
		tpl:      tpl,
		dialOSS: func(cfg config.Config) (store, error) {
			data := cfg.Endpoint
			if opts.DataEndpoint != "" {
				data = opts.DataEndpoint
			}
			return ossclient.NewSplit(data, cfg.Endpoint, cfg.Bucket, cfg.AccessKeyID, cfg.AccessKeySecret)
		},
	}
	if opts.Database != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.openDatabaseMode(ctx, *opts.Database); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// NewWithStore is New for tests that already hold a database: the same
// wiring, with the OSS dial seam replaced before the database mode opens.
func NewWithStore(opts Options, dialOSS func(cfg config.Config) (store, error)) (*Server, error) {
	database := opts.Database
	opts.Database = nil
	s, err := New(opts)
	if err != nil {
		return nil, err
	}
	s.dialOSS = dialOSS
	if database != nil {
		s.opts.Database = database
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.openDatabaseMode(ctx, *database); err != nil {
			return nil, err
		}
	}
	return s, nil
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
	mux.HandleFunc("/overview", s.requireSession(s.handleOverview))
	// The fleet, the policy and the gateway are one page now. The three old
	// paths keep answering for a while, because they are what is in everyone's
	// bookmarks and in every link written down before the merge.
	mux.HandleFunc("/machines", movedTo("/overview"))
	mux.HandleFunc("/policy", movedTo("/overview"))
	mux.HandleFunc("/gateway", movedTo("/overview"))
	mux.HandleFunc("/users", s.requireSession(s.handleUsers))
	mux.HandleFunc("/users/detail", s.requireSession(s.handleUserDetail))
	mux.HandleFunc("/sites", s.requireSession(s.handleSites))
	mux.HandleFunc("/settings", s.requireSession(s.handleSettings))
	mux.HandleFunc("/log", s.requireSession(s.handleLog))
	if s.dbm != nil {
		// The fleet-wide publish page is replaced by the version library.
		mux.HandleFunc("/rollout", movedTo("/releases"))
	} else {
		mux.HandleFunc("/rollout", s.requireSession(s.handleRollout))
	}
	mux.HandleFunc("/logs", s.requireSession(s.handleLogs))

	// Every state-changing route is POST + CSRF + redirect (see requirePost).
	mux.HandleFunc("/users/onboard", s.requirePostBack(backToAccount, s.actionAccountOnboard))
	mux.HandleFunc("/users/reopen", s.requirePostBack(backToAccount, s.actionAccountReopen))
	mux.HandleFunc("/users/offboard", s.requirePostBack(backToAccount, s.actionAccountOffboard))
	mux.HandleFunc("/users/delete", s.requirePostBack(backToAccount, s.actionAccountDelete))
	mux.HandleFunc("/users/quota", s.requirePostBack(backToAccount, s.actionAccountQuota))
	mux.HandleFunc("/users/models", s.requirePostBack(backToAccount, s.actionAccountModels))
	mux.HandleFunc("/users/reissue", s.requirePostBack(backToAccount, s.actionAccountReissue))
	mux.HandleFunc("/users/profile", s.requirePostBack(backToAccount, s.actionAccountProfile))
	mux.HandleFunc("/machines/bind", s.requirePost("/overview", s.actionMachineBind))
	mux.HandleFunc("/machines/unbind", s.requirePost("/overview", s.actionMachineUnbind))
	mux.HandleFunc("/machines/restart-codex", s.requirePostNotice("/overview", s.actionMachineRestartCodex))
	mux.HandleFunc("/machines/sync", s.requirePostNotice("/overview", s.actionMachineSync))
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
	if s.dbm == nil {
		mux.HandleFunc("/agent/publish", s.requirePost("/rollout", s.actionAgentPublish))
		mux.HandleFunc("/agent/cancel", s.requirePost("/rollout", s.actionAgentCancel))
		mux.HandleFunc("/codex/publish", s.requirePost("/rollout", s.actionCodexPublish))
		mux.HandleFunc("/codex/cancel", s.requirePost("/rollout", s.actionCodexCancel))
	} else {
		// In the database mode packages come from CI through the bucket and
		// the version library; the old routes, which held a whole package in
		// memory, lead there.
		for _, old := range []string{"/agent/publish", "/agent/cancel", "/codex/publish", "/codex/cancel"} {
			mux.HandleFunc(old, movedTo("/releases"))
		}
	}
	mux.HandleFunc("/machines/forget", s.requirePost("/overview", s.actionMachineForget))

	// Database mode only: accounts, the authenticator, the queue, health.
	if s.dbm != nil {
		mux.HandleFunc("/enrol", s.handleEnrol)
		mux.HandleFunc("/account", s.requireSession(s.handleAccount))
		mux.HandleFunc("/admins", s.requireSession(s.handleAdmins))
		mux.HandleFunc("/admins/create", s.requireSession(s.handleAdminCreate))
		mux.HandleFunc("/admins/disable", s.requirePost("/admins", s.actionAdminSetDisabled(true)))
		mux.HandleFunc("/admins/enable", s.requirePost("/admins", s.actionAdminSetDisabled(false)))
		mux.HandleFunc("/tasks", s.requireSession(s.handleTasks))
		mux.HandleFunc("/tasks/reconcile", s.requirePostNotice("/tasks", s.actionReconcileNow))
		mux.HandleFunc("/audit", s.requireSession(s.handleAudit))
		mux.HandleFunc("/machines/detail", s.requireSession(s.handleMachineDetail))
		mux.HandleFunc("/audit.csv", s.requireSession(s.handleAuditCSV))
		mux.HandleFunc("/usage", s.requireSession(s.handleUsage))
		mux.HandleFunc("/usage.csv", s.requireSession(s.handleUsageCSV))
		mux.HandleFunc("/usage/snapshot-now", s.requirePostNotice("/usage", s.actionUsageSnapshotNow))
		mux.HandleFunc("/releases", s.requireSession(s.handleReleases))
		mux.HandleFunc("/releases/register", s.requirePost("/releases", s.actionReleaseRegister))
		mux.HandleFunc("/releases/scan", s.requirePostNotice("/releases", s.actionReleaseScanNow))
		mux.HandleFunc("/releases/status", s.requirePost("/releases", s.actionReleaseStatus))
		mux.HandleFunc("/releases/global", s.requirePost("/releases", s.actionReleaseGlobal))
		mux.HandleFunc("/releases/global-clear", s.requirePost("/releases", s.actionReleaseGlobalClear))
		mux.HandleFunc("/rollouts", s.requireSession(s.handleRollouts))
		mux.HandleFunc("/rollouts/new", s.requireSession(s.handleRolloutNew))
		mux.HandleFunc("/rollouts/detail", s.requireSession(s.handleRolloutDetail))
		mux.HandleFunc("/rollouts/create", s.requirePostBack(backToRollout, s.actionRolloutCreate))
		mux.HandleFunc("/rollouts/pause", s.requirePostBack(backToRollout, s.actionRolloutPause))
		mux.HandleFunc("/rollouts/resume", s.requirePostBack(backToRollout, s.actionRolloutResume))
		mux.HandleFunc("/rollouts/cancel", s.requirePostBack(backToRollout, s.actionRolloutCancel))
		mux.HandleFunc("/rollouts/exclude", s.requirePostBack(backToRollout, s.actionRolloutExclude))
		mux.HandleFunc("/rollouts/retry", s.requirePostBack(backToRollout, s.actionRolloutRetry))
		mux.HandleFunc("/rollouts/rollback", s.requirePostBack(backToRollout, s.actionRolloutRollback))
		mux.HandleFunc("/healthz", s.handleHealthz)
	}
	// Serve only assets/static, so the templates next to it are never handed
	// out as raw files, and strip the prefix so paths resolve inside it.
	staticFS, err := fs.Sub(assetFS, "assets/static")
	if err != nil {
		// Only reachable if the embedded tree is missing, which is a build-time
		// mistake rather than something to handle at runtime.
		panic("adminweb: embedded assets/static missing: " + err.Error())
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	return s.accessLog(s.secureHeaders(mux))
}

// movedTo answers a retired path with a redirect to its replacement.
//
// 302 rather than 301: a permanent redirect is cached by the browser
// indefinitely, and these paths are meant to be reclaimed once the bookmarks
// have caught up.
func movedTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}
}

// accessLog records one line per request in the unified log.
//
// Method, path and status only: the query string can carry an employee name
// or a file key, and no header is recorded at all, so a session cookie can
// never reach the log. Static assets are skipped -- they are the same handful
// of files on every page and say nothing about what an operator did.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			return
		}
		level := "info"
		if rec.status >= 500 {
			level = "error"
		} else if rec.status >= 400 {
			level = "warn"
		}
		s.events.Ops(level, "http_access", r.Method+" "+r.URL.Path, map[string]any{
			"method": r.Method, "path": r.URL.Path, "status": rec.status,
			"latency_ms": float64(time.Since(start).Microseconds()) / 1000.0,
			"ok":         rec.status < 400,
		})
	})
}

// statusRecorder remembers the status code so the access log can report it.
// A handler that never calls WriteHeader has answered 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }

// Flush and Unwrap keep the wrapper from quietly disabling what the real
// ResponseWriter can do. Wrapping hides every optional interface behind it,
// so without these a streamed response would buffer to the end and
// http.ResponseController (hijacking, per-request deadlines, ReadFrom's
// sendfile path for file downloads) would report the feature as unsupported.
// Unwrap is what the controller follows; Flush is here for the pre-1.20
// callers that still type-assert http.Flusher directly.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

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

	s.events.Ops("info", "platform_event", "console started", map[string]any{
		"version": s.opts.Version, "listen": s.opts.Listen,
	})
	defer s.events.Ops("info", "platform_event", "console stopping", map[string]any{
		"version": s.opts.Version,
	})

	stopWorker := s.runWorker(context.Background())
	defer stopWorker()

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
// inline script; the one script file is embedded in the binary. A cross-site
// script on this origin would be able to drive the console with the
// operator's session.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
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
