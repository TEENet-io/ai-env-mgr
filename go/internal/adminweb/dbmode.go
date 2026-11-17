package adminweb

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/deviceapi"
	"github.com/TEENet-io/ai-env-mgr/internal/directory"
	"github.com/TEENet-io/ai-env-mgr/internal/ecdclient"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
	"github.com/TEENet-io/ai-env-mgr/internal/slsclient"
	"github.com/TEENet-io/ai-env-mgr/internal/worker"
)

// DatabaseOptions turns the console into the database-backed one.
//
// With these set, sign-in is by account and authenticator, every page reads
// the tables, every change is a transaction plus queued work, and the Worker
// runs in this process. OSS and SLS are reached with the server's own
// identity -- never an administrator's -- which is what lets a session carry
// no cloud credentials at all.
type DatabaseOptions struct {
	DSN           string
	MasterKeyFile string

	// The server's OSS identity. On the target host this becomes the RAM
	// instance role; until then a restricted AccessKey injected from the
	// environment.
	OSSAccessKeyID     string
	OSSAccessKeySecret string
	// MockOSSDir, when set, stores objects in a local directory instead of
	// Aliyun OSS so local test environments require zero cloud credentials.
	MockOSSDir string

	// FirstAdminEmail is recorded on the account created at first start.
	FirstAdminEmail string

	// Worker runs the queue in this process. Off only for a second console
	// instance that should serve pages and nothing else.
	Worker bool

	// IdentityMode is how people sign in (AIC-206). "local", the default,
	// is the console's own accounts and passwords. "oidc" is the customer's
	// custom tool; it is refused until the sign-in through it exists.
	IdentityMode string
	// DirectoryURL and DirectoryToken read the custom tool's list of people
	// (GET /internal/directory/accounts). Set, the Worker reads it every
	// five minutes, in either identity mode. The token comes from the
	// environment only.
	DirectoryURL   string
	DirectoryToken string
	// DirectoryAutoOffboard closes the AI workspace account of somebody
	// disabled or deleted in the custom tool, at the reading that sees it
	// (AIC-209 #5). Off, the default, an alert asks a person to do it.
	DirectoryAutoOffboard bool

	// The custom tool as an OpenID Connect provider, for IdentityMode oidc.
	// RedirectURL is this console's /oidc/callback as the tool has it
	// registered; empty means https://<PublicHost>/oidc/callback. ToolName
	// is what the sign-in buttons call the tool. EmergencyAdmin is the one
	// local account that may still sign in with a password, for when the
	// tool is down; every use of it raises an alert.
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string
	ToolName         string
	EmergencyAdmin   string
}

const (
	IdentityLocal = "local"
	IdentityOIDC  = "oidc"
)

// checkIdentity refuses an identity configuration the console cannot honour,
// rather than starting with a sign-in other than the one asked for.
func (o DatabaseOptions) checkIdentity() error {
	switch o.IdentityMode {
	case "", IdentityLocal:
	case IdentityOIDC:
		if o.OIDCIssuer == "" || o.OIDCClientID == "" || o.OIDCClientSecret == "" {
			return errors.New("AIENVMGR_IDENTITY_MODE=oidc needs AIENVMGR_OIDC_ISSUER, _CLIENT_ID and _CLIENT_SECRET")
		}
		// Without the list, somebody disabled in the tool keeps the sessions
		// they already have here.
		if o.DirectoryURL == "" {
			return errors.New("AIENVMGR_IDENTITY_MODE=oidc needs the directory (AIENVMGR_DIRECTORY_URL and _TOKEN)")
		}
	default:
		return fmt.Errorf("AIENVMGR_IDENTITY_MODE=%q: use local or oidc", o.IdentityMode)
	}
	if (o.DirectoryURL == "") != (o.DirectoryToken == "") {
		return errors.New("AIENVMGR_DIRECTORY_URL and AIENVMGR_DIRECTORY_TOKEN are set together or not at all")
	}
	return nil
}

// dbState is everything the database mode adds to the server.
type dbState struct {
	db      *dbstore.DB
	store   *dbstore.Store
	ops     *ops.Service
	auth    *authn.Service
	ring    secrets.Keyring
	objects admincore.Store
	sls     *slsclient.Client
	cloud   *cloudLookup
	// csrfKey derives each session's form token from its cookie. Per process:
	// a restart invalidates open forms once, which costs a reload and nothing
	// else.
	csrfKey []byte
	worker  *worker.Worker
	// directory reads the custom tool's list of people; nil when this
	// console has none configured.
	directory *directory.Client
	// identity is set when people sign in through the custom tool.
	identity *oidcIdentity
	// hub wakes agents long-polling the device API when their configuration
	// changes; devices is that API.
	hub     *deviceapi.Hub
	devices *deviceapi.Server
	// team is the gateway team setting (repo.SettingGatewayTeam), held here
	// so that the gateway clients and the Worker follow a change at once.
	team atomic.Pointer[string]
}

// tenantTeam is the team tenant-admin says the tenant token manages. It must
// agree with a team stored here before: a token for another customer would
// otherwise have this console open accounts in that customer's team. When
// tenant-admin cannot be reached the stored team is used -- every request
// it would make goes through tenant-admin anyway, which checks the team
// itself -- so a gateway outage does not stop the console from starting.
func (s *Server) tenantTeam(stored string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gw := litellm.New(s.opts.GatewayAdminURL, s.opts.GatewayAdminKey)
	team, err := gw.WhoAmI(ctx)
	if err != nil {
		log.Printf("adminweb: tenant-admin whoami failed, using the stored team %q: %v", stored, err)
		return stored, nil
	}
	if stored != "" && stored != team {
		return "", fmt.Errorf("the gateway token is for team %q but this console's gateway team is %q", team, stored)
	}
	return team, nil
}

// teamID is this customer's LiteLLM team on a shared gateway: keys are
// minted into it, employees are made members of it, and the console reads
// only it. Empty for a gateway this customer does not share, and always
// outside database mode, where there is nowhere to set it.
func (s *Server) teamID() string {
	if s.dbm == nil {
		return ""
	}
	if t := s.dbm.team.Load(); t != nil {
		return *t
	}
	return ""
}

// enrolCookie carries a pending authenticator enrolment between the sign-in
// that found no second factor and the page that sets one up.
const enrolCookie = "aem_enrol"

// openDatabaseMode wires the database side of the server. Called from New
// when DatabaseOptions are given.
func (s *Server) openDatabaseMode(ctx context.Context, opts DatabaseOptions) error {
	if opts.MasterKeyFile == "" {
		return errors.New("database mode needs a master key file (AIENVMGR_MASTER_KEY_FILE)")
	}
	if err := opts.checkIdentity(); err != nil {
		return err
	}
	if opts.MockOSSDir == "" {
		if opts.OSSAccessKeyID == "" || opts.OSSAccessKeySecret == "" {
			return errors.New("database mode needs the server's OSS identity (AIENVMGR_OSS_ACCESS_KEY_ID / _SECRET)")
		}
		if s.opts.Bucket == "" || s.opts.Endpoint == "" {
			return errors.New("database mode needs the OSS bucket and endpoint built in")
		}
	}

	ring, err := secrets.NewFileKeyring(opts.MasterKeyFile)
	if err != nil {
		return err
	}
	database, err := dbstore.Open(ctx, opts.DSN)
	if err != nil {
		return err
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		return fmt.Errorf("migrate: %w", err)
	}
	store := dbstore.NewStore(database)

	var objects interface {
		admincore.Store
		Verify() error
	}
	if opts.MockOSSDir != "" {
		objects, err = newLocalDirStore(opts.MockOSSDir)
		if err != nil {
			database.Close()
			return fmt.Errorf("mock OSS dir: %w", err)
		}
	} else {
		cfg := config.Config{
			Endpoint: s.opts.Endpoint, Bucket: s.opts.Bucket,
			AccessKeyID: opts.OSSAccessKeyID, AccessKeySecret: opts.OSSAccessKeySecret,
		}
		objects, err = s.dialOSS(cfg)
		if err != nil {
			database.Close()
			return fmt.Errorf("server OSS identity: %w", err)
		}
	}
	if err := objects.Verify(); err != nil {
		database.Close()
		return fmt.Errorf("server OSS identity cannot reach the bucket: %w", err)
	}

	csrfKey := make([]byte, 32)
	if _, err := rand.Read(csrfKey); err != nil {
		database.Close()
		return err
	}

	st := &dbState{
		db: database, store: store, ring: ring, objects: objects,
		ops:     ops.New(store),
		auth:    authn.New(store.Admins(), ring, s.issuer()),
		csrfKey: csrfKey,
		hub:     deviceapi.NewHub(),
	}
	st.ops.Notifier = st.hub
	if opts.DirectoryURL != "" {
		st.directory = &directory.Client{URL: opts.DirectoryURL, Token: opts.DirectoryToken}
	}
	if opts.IdentityMode == IdentityOIDC {
		st.identity = newOIDCIdentity(opts, s.consoleURL())
	}
	st.devices = &deviceapi.Server{Store: store, Hub: st.hub, Events: s.events, BehindProxy: s.opts.BehindProxy, Bucket: objects}
	if signer, ok := objects.(deviceapi.Presigner); ok {
		st.devices.Objects = signer
	}
	if dc, _, err := repo.LoadDeviceChannelSettings(context.Background(), store.Settings()); err == nil {
		if nets, err := dc.ParsedCIDRs(); err == nil {
			st.devices.SetEnrolCIDRs(nets)
		}
	}
	// Unreadable is fatal: running without the team would name new users
	// and keys outside it, beside another customer's.
	gt, _, err := repo.LoadGatewayTeamSettings(context.Background(), store.Settings())
	if err != nil {
		database.Close()
		return err
	}
	team := gt.TeamID
	if s.opts.GatewayAdminURL != "" {
		team, err = s.tenantTeam(gt.TeamID)
		if err != nil {
			database.Close()
			return err
		}
	}
	st.team.Store(&team)
	if opts.MockOSSDir == "" && s.opts.SLSProject != "" {
		st.sls = slsclient.New(s.opts.SLSEndpoint, s.opts.SLSProject, opts.OSSAccessKeyID, opts.OSSAccessKeySecret)
	}
	if opts.MockOSSDir == "" && s.opts.ECDRegion != "" {
		if client, err := ecdclient.New(opts.OSSAccessKeyID, opts.OSSAccessKeySecret, s.opts.ECDRegion); err == nil {
			st.cloud = newCloudLookup(client)
		}
	}
	s.dbm = st
	s.jobs.persistent = st.store.AdminJobs()
	if recovered, err := s.jobs.persistent.RecoverRunning(ctx, "Admin restarted before this job finished"); err != nil {
		return fmt.Errorf("recover admin jobs: %w", err)
	} else if recovered > 0 {
		log.Printf("adminweb: marked %d interrupted admin job(s) failed", recovered)
	}

	// The chicken and egg: nobody can sign in to create the first account.
	admin, password, created, err := st.auth.EnsureFirstAccount(ctx, "", opts.FirstAdminEmail)
	if err != nil {
		return fmt.Errorf("first administrator: %w", err)
	}
	if created {
		// Not into the log: journald keeps a line for months, and this one
		// would be a working password. It goes into a root-only file next to
		// the master key, to be read once and deleted.
		path := filepath.Join(filepath.Dir(opts.MasterKeyFile), "first-admin.txt")
		body := fmt.Sprintf("user: %s\npassword: %s\n\nSign in, set up the authenticator, change the password, then delete this file.\n",
			admin.Username, password)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return fmt.Errorf("write the first administrator's password to %s: %w", path, err)
		}
		log.Printf("adminweb: first administrator %q created; the password is in %s (read it, then delete the file)",
			admin.Username, path)
	}

	if opts.Worker {
		st.worker = s.buildWorker(st)
	}
	return nil
}

// issuer is what the authenticator app shows for this console.
func (s *Server) issuer() string {
	if s.opts.PublicHost != "" {
		return s.opts.PublicHost
	}
	return "windows-control"
}

// buildWorker wires every handler with the server's own clients.
func (s *Server) buildWorker(st *dbState) *worker.Worker {
	w := worker.New(st.store, worker.Options{
		OnEvent: func(ev worker.Event) {
			fields := map[string]any{
				"task_id": ev.Task.ID, "kind": ev.Task.Kind, "outcome": ev.Outcome,
				"attempts": ev.Task.Attempts, "duration_ms": ev.Duration.Milliseconds(),
			}
			if ev.Err != nil {
				fields["error"] = ev.Err.Error()
			}
			if ev.ExternalRef != "" {
				fields["external_ref"] = ev.ExternalRef
			}
			if ev.Note != "" {
				fields["note"] = ev.Note
			}
			level := "info"
			if ev.Outcome == "failed" || ev.Outcome == "lost" {
				level = "error"
			}
			s.events.Ops(level, "worker_task", ev.Task.Kind+" "+ev.Outcome, fields)
			if ev.Outcome == "failed" {
				// The hook must not block the worker; a short, separate
				// context keeps a slow database from doing so.
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := worker.OpenTaskFailedAlert(ctx, st.store, ev.Task, ev.Err); err != nil {
					s.events.Ops("warn", "alert_open_failed", err.Error(), map[string]any{"task_id": ev.Task.ID})
				}
			}
		},
		SweepEvery: time.Minute,
		// Two seconds between looks at an empty queue: a change an
		// administrator just made reaches the export, and through it the
		// waiting agents, that much sooner. The idle cost is one cheap query.
		Poll: 2 * time.Second,
	})
	var users worker.UserLister

	var gw *litellm.Client
	if s.opts.GatewayURL != "" && s.opts.GatewayAdminKey != "" {
		gw = s.newGateway()
		gw.TeamSource = s.teamID
	}
	if gw != nil {
		w.Register(repo.TaskGatewayProvision, worker.GatewayProvision{Store: st.store, Gateway: gw, Keyring: st.ring, Catalog: gw, TeamSource: s.teamID})
		w.Register(repo.TaskGatewayRevoke, worker.GatewayRevoke{Store: st.store, Gateway: gw})
		w.Register(repo.TaskReconcile, worker.Reconcile{
			Store: st.store, Gateway: gw,
			OnDrift: func(grant repo.Grant, observed string) {
				s.events.Ops("warn", "gateway_drift", grant.KeyAlias+" "+observed, map[string]any{
					"key_alias": grant.KeyAlias, "employee_id": grant.EmployeeID,
					"desired": grant.Desired, "observed": observed,
				})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := worker.OpenDriftAlert(ctx, st.store, grant, observed); err != nil {
					s.events.Ops("warn", "alert_open_failed", err.Error(), map[string]any{"key_alias": grant.KeyAlias})
				}
			},
		})
		users = gw
		w.Register(repo.TaskOSSExport, worker.OSSExport{
			Store: st.store, Keyring: st.ring, Catalog: gw,
			GatewayBaseURL: s.opts.GatewayURL, Notifier: st.hub,
		})
		w.Register(worker.TaskCatalogSync, worker.CatalogSync{
			Store: st.store, Ops: st.ops, Catalog: gw,
		})
	}
	w.Register(worker.TaskAlertEval, worker.AlertEval{Store: st.store, Gateway: users})
	w.Register(worker.TaskAlertNotify, worker.AlertNotify{Store: st.store, Channels: s.alertChannels, BaseURL: s.consoleURL()})
	w.Register(worker.TaskCredentialRotation, worker.CredentialRotation{Store: st.store, Ops: st.ops})
	w.Register(worker.TaskDevicePrune, worker.DevicePrune{Store: st.store})
	w.Register(worker.TaskDataCleanup, worker.DataCleanup{Store: st.store, Objects: st.objects})
	if src, ok := st.objects.(worker.PackageSource); ok {
		w.Register(worker.TaskReleaseScan, &worker.ReleaseScan{Store: st.store, Objects: src, Ops: st.ops})
	}
	audit := worker.AuditPublish{Store: st.store, Sink: s.events, Logstore: logstoreAudit}
	if st.sls != nil {
		audit.Archive = worker.SLSArchive{Client: st.sls}
		// The gateway's llm_call events live in the same logstore the log
		// page reads them from.
		w.Register(worker.TaskUsageSnapshot, worker.UsageSnapshot{
			Store: st.store, Source: worker.SLSUsage{Client: st.sls, Logstore: logstoreAudit},
		})
	}
	w.Register(repo.TaskAuditPublish, audit)
	if st.directory != nil {
		sync := worker.DirectorySync{Store: st.store, Directory: st.directory, Copy: st.db.Directory()}
		if s.opts.Database.DirectoryAutoOffboard {
			sync.Offboard = st.ops.OffboardForDirectory
		}
		w.Register(worker.TaskDirectorySync, sync)
	}
	return w
}

// runWorker starts the queue and the scheduler and returns a stop function
// that waits for them.
func (s *Server) runWorker(ctx context.Context) func() {
	if s.dbm == nil || s.dbm.worker == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	go func() {
		_ = s.dbm.worker.Run(ctx)
		done <- struct{}{}
	}()
	go func() {
		var extra []worker.Periodic
		if s.dbm.directory != nil {
			extra = append(extra, worker.Periodic{Kind: worker.TaskDirectorySync, Every: worker.EveryDirectorySync, MaxAttempts: 1})
		}
		worker.Schedule(ctx, s.dbm.store, func(err error) {
			log.Printf("adminweb: scheduler: %v", err)
		}, extra...)
		done <- struct{}{}
	}()
	return func() {
		cancel()
		<-done
		<-done
	}
}

// --- sessions ---

// dbSession resolves the cookie into a session for the database mode, or nil.
func (s *Server) dbSession(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	admin, err := s.dbm.auth.Session(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return &session{
		be: dbBackend{
			store: s.dbm.store, ops: s.dbm.ops, objects: s.dbm.objects, team: s.teamID(),
			actor: admin.Username, requestID: requestID(r),
		},
		actor:    admin.Username,
		admin:    &admin,
		bucket:   s.opts.Bucket,
		endpoint: s.opts.Endpoint,
		csrf:     s.csrfFor(c.Value),
		cloud:    s.dbm.cloud,
		sls:      s.dbm.sls,
	}
}

// csrfFor derives the form token from the session cookie. Not the cookie
// itself: a form token is embedded in every page, and it must not be the one
// value that also opens the session.
func (s *Server) csrfFor(token string) string {
	sum := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, s.dbm.csrfKey)
	mac.Write(sum[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// requestID ties a request's audit rows to its log line. The proxy in front
// supplies one; a request without one gets a random one so the rows are
// still grouped.
func requestID(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get("CF-Ray")); id != "" {
		return id
	}
	if id := strings.TrimSpace(r.Header.Get("X-Request-ID")); id != "" {
		return id
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.browserUsesTLS(),
	})
}

// --- sign-in ---

func (s *Server) dbLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "login.html", http.StatusBadRequest, s.loginPage("could not read the form"))
		return
	}
	username := formValue(r, "username")
	password := r.PostFormValue("password")
	code := formValue(r, "code")
	if username == "" || password == "" {
		s.render(w, "login.html", http.StatusBadRequest, s.loginPage("fill in the user name and password"))
		return
	}

	// Through the custom tool, a password opens only the emergency account.
	emergency := false
	if s.oidcMode() {
		emergency = s.dbm.identity.emergency != "" && strings.EqualFold(username, s.dbm.identity.emergency)
		if !emergency {
			s.render(w, "login.html", http.StatusForbidden, s.loginPage("請使用"+s.dbm.identity.toolName+"帳號登入"))
			return
		}
	}

	token, admin, err := s.dbm.auth.SignIn(r.Context(), username, password, code, s.clientKey(r), r.UserAgent())
	switch {
	case err == nil:
		s.setSessionCookie(w, token, 0)
		s.recordAccountEvent(r, admin.Username, "admin.signin", "admin", admin.ID, map[string]any{"emergency": emergency})
		if emergency {
			s.alertEmergencySignIn(r, admin.Username)
		}
		http.Redirect(w, r, "/overview", http.StatusSeeOther)
	case errors.Is(err, authn.ErrEnrolmentRequired):
		// The password was right; the account has no authenticator yet. A
		// short-lived signed cookie carries the enrolment across the next
		// two requests and opens nothing else.
		secret, uri, err := s.dbm.auth.BeginEnrolment(admin)
		if err != nil {
			s.render(w, "login.html", http.StatusInternalServerError, s.loginPage(err.Error()))
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: enrolCookie, Value: s.signEnrolment(admin.ID, secret), Path: "/enrol", MaxAge: 600,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.browserUsesTLS(),
		})
		_ = uri
		http.Redirect(w, r, "/enrol", http.StatusSeeOther)
	case errors.Is(err, authn.ErrLockedOut):
		s.render(w, "login.html", http.StatusTooManyRequests, s.loginPage("嘗試次數過多，請稍後再試"))
	case errors.Is(err, authn.ErrDisabled):
		s.render(w, "login.html", http.StatusForbidden, s.loginPage("這個帳號已停用"))
	case errors.Is(err, authn.ErrSignInFailed):
		log.Printf("adminweb: sign-in from %s rejected", s.clientKey(r))
		s.render(w, "login.html", http.StatusUnauthorized, s.loginPage("用戶名、密碼或驗證碼不正確"))
	default:
		log.Printf("adminweb: sign-in: %v", err)
		s.render(w, "login.html", http.StatusServiceUnavailable, s.loginPage("登入暫時不可用，請稍後再試"))
	}
}

func (s *Server) dbLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.dbm.auth.SignOut(r.Context(), c.Value)
	}
	s.setSessionCookie(w, "", -1)
	http.Redirect(w, r, opsSignInPath, http.StatusSeeOther)
}

// signEnrolment packs the pending enrolment into a signed cookie value.
func (s *Server) signEnrolment(adminID, secret string) string {
	payload := adminID + "|" + secret + "|" + strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	mac := hmac.New(sha256.New, s.dbm.csrfKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) openEnrolment(r *http.Request) (adminID, secret string, ok bool) {
	c, err := r.Cookie(enrolCookie)
	if err != nil {
		return "", "", false
	}
	dot := strings.LastIndex(c.Value, ".")
	if dot < 0 {
		return "", "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(c.Value[:dot])
	if err != nil {
		return "", "", false
	}
	want, err := hex.DecodeString(c.Value[dot+1:])
	if err != nil {
		return "", "", false
	}
	mac := hmac.New(sha256.New, s.dbm.csrfKey)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), want) {
		return "", "", false
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 3 {
		return "", "", false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// enrolPage is what the authenticator set-up shows.
type enrolPage struct {
	Username string
	Secret   string // for manual entry into the app
	URI      string // the otpauth:// link, for apps that accept one
	Codes    []string
}

// opsSignInPath is the operators' own way in. Customers sign in at
// /customer/login and are never linked here.
const opsSignInPath = "/ops/login"

// enrolmentURI escapes the account name: it is a person's name, and an
// authenticator refuses a URI with a raw space in it.
func enrolmentURI(issuer, username, secret string) string {
	return "otpauth://totp/" + url.PathEscape(issuer) + ":" + url.PathEscape(username) + "?secret=" + secret + "&issuer=" + url.QueryEscape(issuer)
}

func (s *Server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	adminID, secret, ok := s.openEnrolment(r)
	if !ok {
		http.Redirect(w, r, opsSignInPath+"?err="+"enrolment+expired%3B+sign+in+again", http.StatusSeeOther)
		return
	}
	admin, err := s.dbm.store.Admins().ByID(r.Context(), adminID)
	if err != nil {
		http.Redirect(w, r, opsSignInPath, http.StatusSeeOther)
		return
	}
	data := pageData{DBLogin: true, Enrol: &enrolPage{
		Username: admin.Username, Secret: secret,
		URI: enrolmentURI(s.issuer(), admin.Username, secret),
	}}
	if r.Method != http.MethodPost {
		s.render(w, "enrol.html", http.StatusOK, data)
		return
	}
	if err := r.ParseForm(); err != nil {
		data.Error = "could not read the form"
		s.render(w, "enrol.html", http.StatusBadRequest, data)
		return
	}
	codes, err := s.dbm.auth.CompleteEnrolment(r.Context(), admin, secret, formValue(r, "code"))
	if err != nil {
		data.Error = "驗證碼不正確，請按應用當前顯示的六位數字重試"
		s.render(w, "enrol.html", http.StatusUnauthorized, data)
		return
	}
	// Enrolment is done: the cookie has served its purpose.
	http.SetCookie(w, &http.Cookie{Name: enrolCookie, Value: "", Path: "/enrol", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.browserUsesTLS()})
	s.recordAccountEvent(r, admin.Username, "admin.enrol", "admin", admin.ID, nil)
	data.Enrol.Codes = codes
	data.Enrol.Secret, data.Enrol.URI = "", ""
	s.render(w, "enrol.html", http.StatusOK, data)
}

// --- the signed-in administrator's own account ---

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "account")
	if r.Method != http.MethodPost {
		s.render(w, "account.html", http.StatusOK, data)
		return
	}
	if err := r.ParseForm(); err != nil || r.PostFormValue("csrf") != sess.csrf {
		data.Error = "the form expired; reload the page and try again"
		s.render(w, "account.html", http.StatusBadRequest, data)
		return
	}
	next := r.PostFormValue("new")
	if next != r.PostFormValue("again") {
		data.Error = "兩次輸入的新密碼不一致"
		s.render(w, "account.html", http.StatusBadRequest, data)
		return
	}
	if err := s.dbm.auth.ChangePassword(r.Context(), *sess.admin, r.PostFormValue("current"), next); err != nil {
		if errors.Is(err, authn.ErrSignInFailed) {
			data.Error = "當前密碼不正確"
		} else {
			data.Error = err.Error()
		}
		s.render(w, "account.html", http.StatusBadRequest, data)
		return
	}
	// Every session of this account is now gone, this one included.
	s.setSessionCookie(w, "", -1)
	s.recordAccountEvent(r, sess.actor, "admin.password_change", "admin", sess.admin.ID, nil)
	http.Redirect(w, r, opsSignInPath+"?msg="+"密碼已修改，請重新登入", http.StatusSeeOther)
}

// --- administrators ---

type adminRow struct {
	Username      string
	Email         string
	Role          string
	Enrolled      bool
	RecoveryCodes int
	LastLogin     string
	Enabled       bool
	Self          bool
	ID            string
	// Linked is the custom tool's ops account this console account is bound
	// to, in the oidc mode: the whitelist of the ops entry.
	Linked string
}

// pendingOpsRow is an ops account of the tool on the "待開通" list.
type pendingOpsRow struct {
	dbstore.PendingOps
	Suggested string // console user name offered
	Requested string // when it last tried to sign in, if it did
}

func (s *Server) handleAdmins(w http.ResponseWriter, r *http.Request, sess *session) {
	s.render(w, "admins.html", http.StatusOK, s.adminsPage(r, sess))
}

func (s *Server) adminsPage(r *http.Request, sess *session) pageData {
	data := newPage(sess, r, "settings")
	data.Tab = "admins"
	admins, err := s.dbm.store.Admins().List(r.Context())
	if err != nil {
		data.Error = "could not list administrators"
		return data
	}
	var links map[string]directory.Account
	if s.oidcMode() {
		links, _ = s.dbm.db.Directory().AdminLinks(r.Context())
		data.OpsAccounts, _ = s.dbm.db.Directory().OpsAccounts(r.Context())
		pending, _ := s.dbm.db.Directory().PendingOpsAccounts(r.Context())
		for _, p := range pending {
			row := pendingOpsRow{PendingOps: p, Suggested: suggestedUsername(p.Username)}
			if p.RequestedAt != nil {
				row.Requested = p.RequestedAt.In(shanghai()).Format("2006-01-02 15:04")
			}
			data.PendingOps = append(data.PendingOps, row)
		}
	}
	for _, a := range admins {
		row := adminRow{
			Username: a.Username, Email: a.Email, Role: a.Role, Enrolled: a.TOTPEnrolled(),
			RecoveryCodes: len(a.RecoveryHashes), Enabled: a.Enabled(), Self: a.Username == sess.actor, ID: a.ID,
		}
		if d, ok := links[a.ID]; ok {
			row.Linked = d.Username
		}
		if a.LastLoginAt != nil {
			row.LastLogin = a.LastLoginAt.In(shanghai()).Format("2006-01-02 15:04")
		}
		data.Admins = append(data.Admins, row)
		if s.oidcMode() && row.Linked == "" && row.Enabled && a.Username != s.dbm.identity.emergency {
			data.Bindable = append(data.Bindable, a.Username)
		}
	}
	return data
}

// handleAdminCreate is a POST like the others, except for what it produces:
// a generated password. That must not travel through a redirect's query
// string -- browser history, the proxy's logs and the next page's Referer
// would all keep it -- so the result is rendered straight from the POST.
func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admins", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil || !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) {
		s.redirectWithError(w, r, "/admins", "the form expired; reload the page and try again")
		return
	}
	admin, password, err := s.actionAdminCreate(sess, r)
	if err != nil {
		s.redirectWithError(w, r, "/admins", err.Error())
		return
	}
	data := s.adminsPage(r, sess)
	data.NewUsername, data.NewPassword = admin.Username, password
	s.render(w, "admins.html", http.StatusOK, data)
}

func (s *Server) actionAdminCreate(sess *session, r *http.Request) (repo.Admin, string, error) {
	if sess.admin.Role != repo.RoleAdmin {
		return repo.Admin{}, "", errors.New("只有 admin 角色可以添加管理員")
	}
	username := formValue(r, "username")
	if username == "" {
		return repo.Admin{}, "", errors.New("a user name is required")
	}
	role := formValue(r, "role")
	if role == "" {
		role = repo.RoleAdmin
	}
	if role != repo.RoleAdmin && role != repo.RoleViewer {
		return repo.Admin{}, "", errors.New("只能建立 admin 或 viewer 帳號")
	}
	admin, password, err := s.dbm.auth.CreateAccount(r.Context(), username, formValue(r, "email"), role)
	if err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			return repo.Admin{}, "", errors.New("這個用戶名已經存在")
		}
		return repo.Admin{}, "", err
	}
	s.recordAccountEvent(r, sess.actor, "admin.create", "admin", admin.ID, map[string]any{
		"username": admin.Username, "email": admin.Email, "role": admin.Role,
	})
	return admin, password, nil
}

// handleAdminReset gives another administrator a new password and removes
// their authenticator, for a forgotten password or a lost phone. Like
// handleAdminCreate, the password is rendered straight from the POST.
func (s *Server) handleAdminReset(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admins", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil || !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) {
		s.redirectWithError(w, r, "/admins", "the form expired; reload the page and try again")
		return
	}
	target, password, err := s.actionAdminReset(sess, r)
	if err != nil {
		s.redirectWithError(w, r, "/admins", err.Error())
		return
	}
	data := s.adminsPage(r, sess)
	data.NewUsername, data.NewPassword, data.NewIsReset = target.Username, password, true
	s.render(w, "admins.html", http.StatusOK, data)
}

func (s *Server) actionAdminReset(sess *session, r *http.Request) (repo.Admin, string, error) {
	if sess.admin.Role != repo.RoleAdmin {
		return repo.Admin{}, "", errors.New("只有 admin 角色可以重置管理員的登入")
	}
	username := formValue(r, "username")
	// Resetting oneself would trade the second factor for a password alone;
	// one's own password is changed on /account, which asks for the old one.
	if username == sess.actor {
		return repo.Admin{}, "", errors.New("不能重置自己；修改自己的密碼請到「我的帳號」")
	}
	target, err := s.dbm.store.Admins().ByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return repo.Admin{}, "", errors.New("找不到這個管理員")
		}
		return repo.Admin{}, "", err
	}
	if !strings.EqualFold(formValue(r, "confirm"), target.Username) {
		return repo.Admin{}, "", errors.New("確認用戶名不一致，未重置")
	}
	password, err := s.dbm.auth.ResetAccount(r.Context(), target)
	if err != nil {
		return repo.Admin{}, "", err
	}
	s.recordAccountEvent(r, sess.actor, "admin.reset", "admin", target.ID, map[string]any{"username": target.Username})
	return target, password, nil
}

func (s *Server) actionAdminSetDisabled(disabled bool) func(*session, *http.Request) error {
	return func(sess *session, r *http.Request) error {
		if sess.admin.Role != repo.RoleAdmin {
			return errors.New("只有 admin 角色可以停用或啟用管理員")
		}
		username := formValue(r, "username")
		if username == sess.actor && disabled {
			return errors.New("不能停用自己")
		}
		target, err := s.dbm.store.Admins().ByUsername(r.Context(), username)
		if err != nil {
			return err
		}
		if _, err := s.dbm.store.Admins().SetDisabled(r.Context(), target.ID, disabled); err != nil {
			return err
		}
		if disabled {
			_, _ = s.dbm.store.Admins().DeleteSessionsFor(r.Context(), target.ID)
		}
		action := "admin.enable"
		if disabled {
			action = "admin.disable"
		}
		s.recordAccountEvent(r, sess.actor, action, "admin", target.ID, map[string]any{"username": target.Username})
		return nil
	}
}

// --- tasks ---

type taskRow struct {
	ID        string
	Kind      string
	Detail    string
	Status    string
	Attempts  int
	NextRun   string
	Updated   string
	LastError string
	Employee  string
	Open      bool
}

var hiddenTaskKinds = map[string]bool{
	"audit_publish": true,
	"alert_notify":  true,
	"status_import": true,
	"data_cleanup":  true,
}

type applicationTaskRow struct {
	Machine, AppID, Version, TaskID, Desired, State, Updated, LastError string
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "tasks")
	data.TaskColumns = buildTaskBoard(nil, nil)
	data.TaskKinds = taskKindOptions
	data.TaskStatusFilter = strings.TrimSpace(r.URL.Query().Get("status"))
	data.TaskKindFilter = strings.TrimSpace(r.URL.Query().Get("kind"))
	data.TaskShowSystem = r.URL.Query().Get("system") == "1"
	data.TaskPage, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if data.TaskPage < 1 {
		data.TaskPage = 1
	}
	data.TaskPageSize = 50
	pageStart, pageEnd := (data.TaskPage-1)*data.TaskPageSize, data.TaskPage*data.TaskPageSize
	// Read enough of each source to fill the requested page. The repository
	// APIs predate cursor pagination, so keep a bounded ceiling rather than
	// silently making page 5 empty after the old 200-row window.
	fetchLimit := pageEnd
	if fetchLimit < data.TaskPageSize {
		fetchLimit = data.TaskPageSize
	}
	if fetchLimit > 5000 {
		fetchLimit = 5000
	}
	data.TaskQuery = r.URL.RawQuery
	data.TaskRefreshed = time.Now().In(shanghai()).Format("01-02 15:04:05")
	data.Tab = "tasks"
	tasks, err := s.dbm.store.Tasks().ListRecent(r.Context(), fetchLimit)
	if err != nil {
		data.Error = "could not list tasks"
		s.render(w, "tasks.html", http.StatusOK, data)
		return
	}
	names := map[string]string{}
	deviceNames := map[string]string{}
	if devices, derr := s.dbm.store.Devices().List(r.Context(), repo.DeviceFilter{IncludeRevoked: true}); derr == nil {
		for _, d := range devices {
			deviceNames[d.ID] = d.Hostname
		}
	}
	for _, t := range tasks {
		if t.Open() {
			data.TaskLive = true
		}
		// Choosing a specific maintenance kind is an explicit request to see
		// it; the checkbox is only needed when browsing all task types.
		if hiddenTaskKinds[t.Kind] && !data.TaskShowSystem && data.TaskKindFilter == "" {
			data.TaskHidden++
			continue
		}
		if data.TaskKindFilter != "" && data.TaskKindFilter != t.Kind {
			continue
		}
		if !taskStatusMatches(string(t.Status), data.TaskStatusFilter) {
			continue
		}
		row := taskRow{
			ID: t.ID, Kind: t.Kind, Status: string(t.Status), Attempts: t.Attempts,
			LastError: t.LastError, Open: t.Open(),
			Updated: t.UpdatedAt.In(shanghai()).Format("01-02 15:04:05"),
		}
		if t.Open() {
			row.NextRun = t.NextRunAt.In(shanghai()).Format("01-02 15:04:05")
		}
		if t.EmployeeID != "" {
			name, ok := names[t.EmployeeID]
			if !ok {
				if e, err := s.dbm.store.Employees().ByID(r.Context(), t.EmployeeID); err == nil {
					name = e.WindowsUser
				}
				names[t.EmployeeID] = name
			}
			row.Employee = name
		}
		if data.TaskTotal >= pageStart && data.TaskTotal < pageEnd {
			data.Tasks = append(data.Tasks, row)
		}
		data.TaskTotal++
	}
	if jobs, err := s.dbm.store.AdminJobs().ListRecent(r.Context(), fetchLimit); err == nil {
		for _, job := range jobs {
			if job.State == "running" {
				data.TaskLive = true
			}
			if data.TaskKindFilter != "" && data.TaskKindFilter != "admin_job" {
				continue
			}
			if !taskStatusMatches(job.State, data.TaskStatusFilter) {
				continue
			}
			row := taskRow{
				ID: job.ID, Kind: "admin_job", Detail: job.Kind + " · " + job.Version, Status: job.State,
				Updated:   job.UpdatedAt.In(shanghai()).Format("01-02 15:04:05"),
				LastError: job.LastError, Employee: "管理員操作",
			}
			if job.Step != "" {
				row.Detail += " · " + job.Step
			}
			if data.TaskTotal >= pageStart && data.TaskTotal < pageEnd {
				data.Tasks = append(data.Tasks, row)
			}
			data.TaskTotal++
		}
	}
	// PostgreSQL owns the install lifecycle. OSS contains only immutable
	// manifests/packages; a stale machine plan must never resurrect a task.
	if appTasks, err := s.dbm.store.ApplicationTasks().ListRecent(r.Context(), fetchLimit); err == nil {
		for _, task := range appTasks {
			if task.State == "pending" || task.State == "running" {
				data.TaskLive = true
			}
			if data.TaskKindFilter != "" && data.TaskKindFilter != "application" {
				continue
			}
			if !taskStatusMatches(task.State, data.TaskStatusFilter) {
				continue
			}
			machine := task.DeviceID
			if name := deviceNames[task.DeviceID]; name != "" {
				machine = name
			}
			appRow := applicationTaskRow{
				Machine: machine, AppID: task.AppID, Version: task.Version, TaskID: task.ID,
				Desired: "installed", State: task.State, Updated: task.UpdatedAt.In(shanghai()).Format("01-02 15:04:05"), LastError: task.LastError,
			}
			if data.TaskTotal >= pageStart && data.TaskTotal < pageEnd {
				data.ApplicationTasks = append(data.ApplicationTasks, appRow)
			}
			data.TaskTotal++
		}
	}
	data.TaskHasPrev = data.TaskPage > 1
	data.TaskHasNext = data.TaskTotal > pageEnd
	data.TaskColumns = buildTaskBoard(data.Tasks, data.ApplicationTasks)
	s.render(w, "tasks.html", http.StatusOK, data)
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request, sess *session) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		data := newPage(sess, r, "tasks")
		data.Error = "缺少任務編號"
		s.render(w, "task_detail.html", http.StatusBadRequest, data)
		return
	}
	data := newPage(sess, r, "tasks")
	data.Tab = "tasks"
	view := &taskDetailView{ID: id}
	if task, err := s.dbm.store.Tasks().ByID(r.Context(), id); err == nil {
		view.Kind, view.Status = task.Kind, string(task.Status)
		view.Detail = task.IdempotencyKey
		view.Created = task.CreatedAt.In(shanghai()).Format("2006-01-02 15:04:05")
		view.Updated = task.UpdatedAt.In(shanghai()).Format("2006-01-02 15:04:05")
		if task.FinishedAt != nil {
			view.Finished = task.FinishedAt.In(shanghai()).Format("2006-01-02 15:04:05")
		}
		view.Error = task.LastError
		view.CanRetry = task.Status == repo.TaskFailed
		view.CanCancel = task.Open()
		if task.EmployeeID != "" {
			if e, eerr := s.dbm.store.Employees().ByID(r.Context(), task.EmployeeID); eerr == nil {
				view.Subject = e.WindowsUser
			}
		}
		if task.DeviceID != "" {
			if d, derr := s.dbm.store.Devices().ByID(r.Context(), task.DeviceID); derr == nil {
				view.Machine = d.Hostname
				if view.Subject == "系統任務" {
					view.Subject = d.Hostname
				}
			}
		}
		if view.Subject == "" {
			view.Subject = "系統任務"
		}
		if attempts, aerr := s.dbm.store.Tasks().Attempts(r.Context(), id); aerr == nil {
			for _, a := range attempts {
				av := taskAttemptView{Attempt: a.Attempt, Owner: a.Owner, Outcome: a.Outcome, Error: a.ErrorDetail, Started: a.StartedAt.In(shanghai()).Format("01-02 15:04:05")}
				if a.EndedAt != nil {
					av.Ended = a.EndedAt.In(shanghai()).Format("01-02 15:04:05")
				}
				view.Attempts = append(view.Attempts, av)
			}
		}
		data.TaskDetail = view
		s.render(w, "task_detail.html", http.StatusOK, data)
		return
	}
	if task, err := s.dbm.store.ApplicationTasks().ByID(r.Context(), id); err == nil {
		view.Kind, view.Status, view.Detail, view.Error = "application", task.State, task.AppID+" · "+task.Version, task.LastError
		view.Created = task.CreatedAt.In(shanghai()).Format("2006-01-02 15:04:05")
		view.Updated = task.UpdatedAt.In(shanghai()).Format("2006-01-02 15:04:05")
		if task.FinishedAt != nil {
			view.Finished = task.FinishedAt.In(shanghai()).Format("2006-01-02 15:04:05")
		}
		view.CanRetry = task.State == "failed"
		view.CanCancel = task.State == "pending" || task.State == "running"
		if d, derr := s.dbm.store.Devices().ByID(r.Context(), task.DeviceID); derr == nil {
			view.Subject = d.Hostname
			view.Machine = d.Hostname
		}
		data.TaskDetail = view
		s.render(w, "task_detail.html", http.StatusOK, data)
		return
	}
	if jobs, err := s.dbm.store.AdminJobs().ListRecent(r.Context(), 500); err == nil {
		for _, job := range jobs {
			if job.ID != id {
				continue
			}
			view.Kind, view.Status, view.Subject = job.Kind, job.State, "管理員操作"
			view.Detail = job.Version + " · " + job.Step
			view.Error = job.LastError
			view.Created = job.StartedAt.In(shanghai()).Format("2006-01-02 15:04:05")
			view.Updated = job.UpdatedAt.In(shanghai()).Format("2006-01-02 15:04:05")
			if job.EndedAt != nil {
				view.Finished = job.EndedAt.In(shanghai()).Format("2006-01-02 15:04:05")
			}
			data.TaskDetail = view
			s.render(w, "task_detail.html", http.StatusOK, data)
			return
		}
	}
	data.Error = "找不到這個任務，可能已被清理"
	s.render(w, "task_detail.html", http.StatusNotFound, data)
}

func (s *Server) actionTaskRetry(_ *session, r *http.Request) error {
	id := strings.TrimSpace(formValue(r, "id"))
	// Application tasks have their own lease/state table; retrying one must
	// create a new durable install task rather than looking in the generic
	// worker queue.
	if app, err := s.dbm.store.ApplicationTasks().ByID(r.Context(), id); err == nil {
		if app.State != "failed" {
			return fmt.Errorf("只有失敗任務可以重試")
		}
		_, err := s.dbm.store.ApplicationTasks().Create(r.Context(), app.DeviceID, app.AppID, app.Version, "admin-retry", app.AllowDowngrade)
		return err
	}
	t, err := s.dbm.store.Tasks().ByID(r.Context(), id)
	if err != nil {
		return err
	}
	if t.Status != repo.TaskFailed {
		return fmt.Errorf("只有失敗任務可以重試")
	}
	_, _, err = s.dbm.store.Tasks().Enqueue(r.Context(), repo.NewTask{Kind: t.Kind, IdempotencyKey: t.IdempotencyKey, Payload: t.Payload, TargetEpoch: t.TargetEpoch, EmployeeID: t.EmployeeID, DeviceID: t.DeviceID, MaxAttempts: t.MaxAttempts})
	return err
}

func (s *Server) actionTaskCancel(_ *session, r *http.Request) error {
	id := strings.TrimSpace(formValue(r, "id"))
	if task, err := s.dbm.store.ApplicationTasks().ByID(r.Context(), id); err == nil {
		if task.State != "pending" && task.State != "running" {
			return fmt.Errorf("這個安裝任務已經結束")
		}
		_, err = s.dbm.store.ApplicationTasks().Cancel(r.Context(), id, task.DeviceID)
		return err
	}
	if task, err := s.dbm.store.Tasks().ByID(r.Context(), id); err == nil {
		if !task.Open() {
			return fmt.Errorf("這個任務已經結束")
		}
		return s.dbm.store.Tasks().Supersede(r.Context(), id, "cancelled by administrator")
	}
	return repo.ErrNotFound
}

func (s *Server) actionReconcileNow(_ *session, r *http.Request) (string, error) {
	if err := worker.EnqueueReconcile(r.Context(), s.dbm.store, time.Now()); err != nil {
		return "", err
	}
	return "已排隊，Worker 會在幾秒內開始對帳", nil
}

// actionReleaseScanNow queues a bucket scan for an administrator who knows
// CI has just uploaded something and does not want to wait for the daily one.
func (s *Server) actionReleaseScanNow(_ *session, r *http.Request) (string, error) {
	if err := worker.EnqueueReleaseScan(r.Context(), s.dbm.store, time.Now()); err != nil {
		return "", err
	}
	return "已排隊，Worker 幾秒內開始掃描 OSS；新包算完 SHA-256 後出現在版本庫裡，刷新本頁查看", nil
}

// handleHealthz answers the proxy and the service manager: the database is
// reachable, or it is not.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if s.dbm != nil {
		if err := s.dbm.db.Ping(ctx); err != nil {
			http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}
