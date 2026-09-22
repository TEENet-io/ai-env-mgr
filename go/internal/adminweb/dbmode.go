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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/deviceapi"
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

	// FirstAdminEmail is recorded on the account created at first start.
	FirstAdminEmail string

	// Worker runs the queue in this process. Off only for a second console
	// instance that should serve pages and nothing else.
	Worker bool
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
	// hub wakes agents long-polling the device API when their configuration
	// changes; devices is that API.
	hub     *deviceapi.Hub
	devices *deviceapi.Server
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
	if opts.OSSAccessKeyID == "" || opts.OSSAccessKeySecret == "" {
		return errors.New("database mode needs the server's OSS identity (AIENVMGR_OSS_ACCESS_KEY_ID / _SECRET)")
	}
	if s.opts.Bucket == "" || s.opts.Endpoint == "" {
		return errors.New("database mode needs the OSS bucket and endpoint built in")
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

	cfg := config.Config{
		Endpoint: s.opts.Endpoint, Bucket: s.opts.Bucket,
		AccessKeyID: opts.OSSAccessKeyID, AccessKeySecret: opts.OSSAccessKeySecret,
	}
	objects, err := s.dialOSS(cfg)
	if err != nil {
		database.Close()
		return fmt.Errorf("server OSS identity: %w", err)
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
	st.devices = &deviceapi.Server{Store: store, Hub: st.hub, Events: s.events, BehindProxy: s.opts.BehindProxy}
	if signer, ok := objects.(deviceapi.Presigner); ok {
		st.devices.Objects = signer
	}
	if dc, _, err := repo.LoadDeviceChannelSettings(context.Background(), store.Settings()); err == nil {
		if nets, err := dc.ParsedCIDRs(); err == nil {
			st.devices.SetEnrolCIDRs(nets)
		}
	}
	if s.opts.SLSProject != "" {
		st.sls = slsclient.New(s.opts.SLSEndpoint, s.opts.SLSProject, opts.OSSAccessKeyID, opts.OSSAccessKeySecret)
	}
	if s.opts.ECDRegion != "" {
		if client, err := ecdclient.New(opts.OSSAccessKeyID, opts.OSSAccessKeySecret, s.opts.ECDRegion); err == nil {
			st.cloud = newCloudLookup(client)
		}
	}
	s.dbm = st

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
		gw = litellm.New(s.opts.GatewayURL, s.opts.GatewayAdminKey)
	}
	if gw != nil {
		w.Register(repo.TaskGatewayProvision, worker.GatewayProvision{Store: st.store, Gateway: gw, Keyring: st.ring})
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
			Store: st.store, Objects: st.objects, Keyring: st.ring, Catalog: gw,
			GatewayBaseURL: s.opts.GatewayURL, Notifier: st.hub,
		})
	}
	w.Register(worker.TaskStatusImport, worker.StatusImport{Store: st.store, Objects: st.objects})
	w.Register(worker.TaskAlertEval, worker.AlertEval{Store: st.store, Gateway: users})
	w.Register(worker.TaskAlertNotify, worker.AlertNotify{Store: st.store, Channels: s.alertChannels, BaseURL: s.consoleURL()})
	w.Register(worker.TaskCredentialRotation, worker.CredentialRotation{Store: st.store, Ops: st.ops})
	w.Register(worker.TaskDevicePrune, worker.DevicePrune{Store: st.store})
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
		worker.Schedule(ctx, s.dbm.store, func(err error) {
			log.Printf("adminweb: scheduler: %v", err)
		})
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
			store: s.dbm.store, ops: s.dbm.ops, objects: s.dbm.objects,
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

	token, admin, err := s.dbm.auth.SignIn(r.Context(), username, password, code, s.clientKey(r), r.UserAgent())
	switch {
	case err == nil:
		s.setSessionCookie(w, token, 0)
		s.events.Audit("admin_signin", "administrator signed in", map[string]any{"user": admin.Username})
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
		s.render(w, "login.html", http.StatusTooManyRequests, s.loginPage("尝试次数过多，请稍后再试"))
	case errors.Is(err, authn.ErrDisabled):
		s.render(w, "login.html", http.StatusForbidden, s.loginPage("这个账号已停用"))
	case errors.Is(err, authn.ErrSignInFailed):
		log.Printf("adminweb: sign-in from %s rejected", s.clientKey(r))
		s.render(w, "login.html", http.StatusUnauthorized, s.loginPage("用户名、密码或验证码不正确"))
	default:
		log.Printf("adminweb: sign-in: %v", err)
		s.render(w, "login.html", http.StatusServiceUnavailable, s.loginPage("登录暂时不可用，请稍后再试"))
	}
}

func (s *Server) dbLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.dbm.auth.SignOut(r.Context(), c.Value)
	}
	s.setSessionCookie(w, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
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

func (s *Server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	adminID, secret, ok := s.openEnrolment(r)
	if !ok {
		http.Redirect(w, r, "/?err="+"enrolment+expired%3B+sign+in+again", http.StatusSeeOther)
		return
	}
	admin, err := s.dbm.store.Admins().ByID(r.Context(), adminID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data := pageData{DBLogin: true, Enrol: &enrolPage{
		Username: admin.Username, Secret: secret,
		URI: "otpauth://totp/" + s.issuer() + ":" + admin.Username + "?secret=" + secret + "&issuer=" + s.issuer(),
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
		data.Error = "验证码不正确，请按应用当前显示的六位数字重试"
		s.render(w, "enrol.html", http.StatusUnauthorized, data)
		return
	}
	// Enrolment is done: the cookie has served its purpose.
	http.SetCookie(w, &http.Cookie{Name: enrolCookie, Value: "", Path: "/enrol", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.browserUsesTLS()})
	s.events.Audit("admin_enrolled", "administrator set up an authenticator", map[string]any{"user": admin.Username})
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
		data.Error = "两次输入的新密码不一致"
		s.render(w, "account.html", http.StatusBadRequest, data)
		return
	}
	if err := s.dbm.auth.ChangePassword(r.Context(), *sess.admin, r.PostFormValue("current"), next); err != nil {
		if errors.Is(err, authn.ErrSignInFailed) {
			data.Error = "当前密码不正确"
		} else {
			data.Error = err.Error()
		}
		s.render(w, "account.html", http.StatusBadRequest, data)
		return
	}
	// Every session of this account is now gone, this one included.
	s.setSessionCookie(w, "", -1)
	s.events.Audit("admin_password_changed", "administrator changed their password", map[string]any{"user": sess.actor})
	http.Redirect(w, r, "/?msg="+"密码已修改，请重新登录", http.StatusSeeOther)
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
	for _, a := range admins {
		row := adminRow{
			Username: a.Username, Email: a.Email, Role: a.Role, Enrolled: a.TOTPEnrolled(),
			RecoveryCodes: len(a.RecoveryHashes), Enabled: a.Enabled(), Self: a.Username == sess.actor,
		}
		if a.LastLoginAt != nil {
			row.LastLogin = a.LastLoginAt.In(shanghai()).Format("2006-01-02 15:04")
		}
		data.Admins = append(data.Admins, row)
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
		return repo.Admin{}, "", errors.New("只有 admin 角色可以添加管理员")
	}
	username := formValue(r, "username")
	if username == "" {
		return repo.Admin{}, "", errors.New("a user name is required")
	}
	role := formValue(r, "role")
	if role == "" {
		role = repo.RoleAdmin
	}
	admin, password, err := s.dbm.auth.CreateAccount(r.Context(), username, formValue(r, "email"), role)
	if err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			return repo.Admin{}, "", errors.New("这个用户名已经存在")
		}
		return repo.Admin{}, "", err
	}
	s.events.Audit("admin_created", "administrator account created", map[string]any{
		"user": admin.Username, "role": admin.Role, "by": sess.actor,
	})
	return admin, password, nil
}

func (s *Server) actionAdminSetDisabled(disabled bool) func(*session, *http.Request) error {
	return func(sess *session, r *http.Request) error {
		if sess.admin.Role != repo.RoleAdmin {
			return errors.New("只有 admin 角色可以停用或启用管理员")
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
		s.events.Audit("admin_disabled", "administrator account toggled", map[string]any{
			"user": target.Username, "disabled": disabled, "by": sess.actor,
		})
		return nil
	}
}

// --- tasks ---

type taskRow struct {
	ID        string
	Kind      string
	Status    string
	Attempts  int
	NextRun   string
	Updated   string
	LastError string
	Employee  string
	Open      bool
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "alerts")
	data.Tab = "tasks"
	tasks, err := s.dbm.store.Tasks().ListRecent(r.Context(), 200)
	if err != nil {
		data.Error = "could not list tasks"
		s.render(w, "tasks.html", http.StatusOK, data)
		return
	}
	names := map[string]string{}
	for _, t := range tasks {
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
		data.Tasks = append(data.Tasks, row)
	}
	s.render(w, "tasks.html", http.StatusOK, data)
}

func (s *Server) actionReconcileNow(_ *session, r *http.Request) (string, error) {
	if err := worker.EnqueueReconcile(r.Context(), s.dbm.store, time.Now()); err != nil {
		return "", err
	}
	return "已排队，Worker 会在几秒内开始对账", nil
}

// actionReleaseScanNow queues a bucket scan for an administrator who knows
// CI has just uploaded something and does not want to wait for the daily one.
func (s *Server) actionReleaseScanNow(_ *session, r *http.Request) (string, error) {
	if err := worker.EnqueueReleaseScan(r.Context(), s.dbm.store, time.Now()); err != nil {
		return "", err
	}
	return "已排队，Worker 几秒内开始扫描 OSS；新包算完 SHA-256 后出现在版本库里，刷新本页查看", nil
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
