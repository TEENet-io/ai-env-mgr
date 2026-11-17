package adminweb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/directory"
	"github.com/TEENet-io/ai-env-mgr/internal/oidc"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Signing in through the customer's custom tool (AIC-206, docs/账号打通改造计划
// 3.1). Both entries send the browser to the tool and come back to
// /oidc/callback; what the person may then do depends on the entry they
// started from:
//
//   - the customer entry lets in the tool's admin and employee accounts, as
//     ticket contacts, and makes the contact on the spot for somebody the
//     last reading of the list did not have yet;
//   - the ops entry lets in an ops account that passed the tool's second
//     factor within the last opsReauth and is linked to a console account
//     here -- the link is the whitelist. One that is not linked yet is put
//     on the "待開通" list of /admins, where an administrator opens it with
//     one click.
//
// A refusal says only that the account may not come in, never why; the
// reason goes to the audit trail, and at the ops entry to an alert. The one
// exception is an ops account waiting to be opened, which is told so: it has
// already proved who it is, and it should know somebody has been asked.

const (
	entryCustomer = "customer"
	entryOps      = "ops"

	oidcStateCookie = "aem_oidc"
	oidcFlowTTL     = 10 * time.Minute
	refusedMessage  = "此帳號無權進入"
	pendingMessage  = "帳號尚未開通，已通知管理員；開通後重新登入即可"

	// opsReauth is how long ago an operator may have given the password and
	// code for the ops entry to take the tool's word without asking again:
	// long enough not to ask twice in a row, short enough that a tool
	// session left open on some desk does not open the console.
	opsReauth = 10 * time.Minute
)

type oidcIdentity struct {
	provider    *oidc.Provider
	redirectURL string
	toolName    string
	emergency   string

	mu    sync.Mutex
	flows map[string]pendingSignIn
}

type pendingSignIn struct {
	flow  oidc.Flow
	entry string
	at    time.Time
}

func newOIDCIdentity(opts DatabaseOptions, consoleURL string) *oidcIdentity {
	redirect := opts.OIDCRedirectURL
	if redirect == "" {
		redirect = consoleURL + "/oidc/callback"
	}
	name := opts.ToolName
	if name == "" {
		name = "定製工具"
	}
	return &oidcIdentity{
		provider:    &oidc.Provider{Issuer: opts.OIDCIssuer, ClientID: opts.OIDCClientID, ClientSecret: opts.OIDCClientSecret},
		redirectURL: redirect,
		toolName:    name,
		emergency:   strings.ToLower(strings.TrimSpace(opts.EmergencyAdmin)),
		flows:       map[string]pendingSignIn{},
	}
}

// base is this console's own address as the tool sees it, for the pages
// the tool sends people back to after signing out.
func (id *oidcIdentity) base() string {
	u, err := url.Parse(id.redirectURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (id *oidcIdentity) put(p pendingSignIn) {
	id.mu.Lock()
	defer id.mu.Unlock()
	for k, v := range id.flows {
		if time.Since(v.at) > oidcFlowTTL {
			delete(id.flows, k)
		}
	}
	id.flows[p.flow.State] = p
}

func (id *oidcIdentity) take(state string) (pendingSignIn, bool) {
	id.mu.Lock()
	defer id.mu.Unlock()
	p, ok := id.flows[state]
	delete(id.flows, state)
	if !ok || time.Since(p.at) > oidcFlowTTL {
		return pendingSignIn{}, false
	}
	return p, true
}

// oidcMode reports whether people sign in through the custom tool.
func (s *Server) oidcMode() bool { return s.dbm != nil && s.dbm.identity != nil }

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if !s.oidcMode() {
		http.NotFound(w, r)
		return
	}
	entry := r.URL.Query().Get("entry")
	if entry != entryCustomer && entry != entryOps {
		http.Error(w, "unknown entry", http.StatusBadRequest)
		return
	}
	id := s.dbm.identity
	flow, err := oidc.NewFlow()
	if err != nil {
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	var maxAge time.Duration
	if entry == entryOps {
		maxAge = opsReauth
	}
	target, err := id.provider.AuthURL(r.Context(), flow, id.redirectURL, maxAge)
	if err == nil {
		err = id.provider.Reachable(r.Context())
	}
	if err != nil {
		s.events.Ops("error", "oidc_unavailable", err.Error(), nil)
		s.refuseSignIn(w, r, entry, "暫時無法連接"+id.toolName+"，請稍後再試")
		return
	}
	id.put(pendingSignIn{flow: flow, entry: entry, at: time.Now()})
	// Lax, not Strict: the callback is a navigation from the tool, which a
	// Strict cookie would not come back with. It holds only the state, which
	// the callback compares; the verifier and nonce stay on the server.
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: flow.State, Path: "/oidc", MaxAge: int(oidcFlowTTL.Seconds()),
		HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if !s.oidcMode() {
		http.NotFound(w, r)
		return
	}
	id := s.dbm.identity
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: "/oidc", MaxAge: -1,
		HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteLaxMode})
	state := r.URL.Query().Get("state")
	c, err := r.Cookie(oidcStateCookie)
	// The state must be the one this browser started with: a callback
	// carrying somebody else's code would otherwise sign this browser in as
	// them.
	if err != nil || state == "" || c.Value != state {
		http.Redirect(w, r, "/customer/login?err="+url.QueryEscape("登入已過期，請重新登入"), http.StatusSeeOther)
		return
	}
	pending, ok := id.take(state)
	if !ok {
		http.Redirect(w, r, "/customer/login?err="+url.QueryEscape("登入已過期，請重新登入"), http.StatusSeeOther)
		return
	}
	if r.URL.Query().Get("error") != "" {
		s.refuseSignIn(w, r, pending.entry, refusedMessage)
		return
	}
	claims, err := id.provider.Exchange(r.Context(), pending.flow, r.URL.Query().Get("code"), id.redirectURL)
	if err != nil {
		s.events.Ops("warn", "oidc_exchange_failed", err.Error(), map[string]any{"entry": pending.entry})
		s.refuseSignIn(w, r, pending.entry, refusedMessage)
		return
	}
	if pending.entry == entryOps {
		s.oidcOpsSignIn(w, r, claims)
		return
	}
	s.oidcCustomerSignIn(w, r, claims)
}

func claimsAccount(c oidc.Claims) directory.Account {
	return directory.Account{ID: c.Subject, Username: c.PreferredUsername, Name: c.Name, Email: c.Email, Role: c.Role, Status: directory.StatusActive}
}

func (s *Server) oidcCustomerSignIn(w http.ResponseWriter, r *http.Request, c oidc.Claims) {
	a := claimsAccount(c)
	if a.Role != directory.RoleAdmin && a.Role != directory.RoleEmployee {
		s.recordRefusal(r, entryCustomer, c, "role "+a.Role)
		s.refuseSignIn(w, r, entryCustomer, refusedMessage)
		return
	}
	contactID, err := s.dbm.db.Directory().CustomerForSignIn(r.Context(), a)
	if err != nil {
		if !errors.Is(err, dbstore.ErrNotPermitted) {
			s.events.Ops("error", "oidc_contact_failed", err.Error(), map[string]any{"sub": c.Subject})
		}
		s.recordRefusal(r, entryCustomer, c, "no usable contact")
		s.refuseSignIn(w, r, entryCustomer, refusedMessage)
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	if err := s.dbm.db.Tickets().CreateSession(r.Context(), contactID, sum[:], s.opts.IdleTTL, s.opts.AbsTTL); err != nil {
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: customerCookie, Value: token, Path: "/customer", HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteStrictMode})
	s.recordAccountEvent(r, c.PreferredUsername, "contact.signin", "customer_contact", contactID, map[string]any{"via": "oidc"})
	bounce(w, "/customer")
}

func (s *Server) oidcOpsSignIn(w http.ResponseWriter, r *http.Request, c oidc.Claims) {
	why := ""
	switch {
	case c.Role != directory.RoleOps:
		why = "role " + c.Role
	case !c.SecondFactor():
		why = "no second factor"
	case !c.AuthenticatedWithin(opsReauth, time.Now()):
		why = "password not given recently"
	}
	username := ""
	if why == "" {
		var err error
		username, err = s.dbm.db.Directory().AdminForSignIn(r.Context(), c.Subject)
		// Bound to a console account that is disabled here is a refusal;
		// bound to nothing is somebody waiting to be opened.
		if errors.Is(err, dbstore.ErrNotPermitted) && !s.dbm.db.Directory().IsBound(r.Context(), c.Subject) {
			s.awaitOpening(w, r, c)
			return
		}
		if err != nil {
			why = "not on the whitelist"
		}
	}
	if why != "" {
		s.recordRefusal(r, entryOps, c, why)
		s.alertOpsRefusal(r.Context(), c, why)
		s.refuseSignIn(w, r, entryOps, refusedMessage)
		return
	}
	token, admin, err := s.dbm.auth.SignInVerified(r.Context(), username, s.clientKey(r), r.UserAgent())
	if err != nil {
		s.recordRefusal(r, entryOps, c, err.Error())
		s.refuseSignIn(w, r, entryOps, refusedMessage)
		return
	}
	s.setSessionCookie(w, token, 0)
	s.recordAccountEvent(r, admin.Username, "admin.signin", "admin", admin.ID, map[string]any{"via": "oidc", "directory_id": c.Subject})
	bounce(w, "/overview")
}

func (s *Server) recordRefusal(r *http.Request, entry string, c oidc.Claims, why string) {
	s.recordAccountEvent(r, c.PreferredUsername, "signin.refused", "directory_account", c.Subject, map[string]any{
		"entry": entry, "role": c.Role, "reason": why,
	})
}

// awaitOpening: an ops account that proved itself and is bound to nothing
// here. It goes on the waiting list of /admins and the others are told.
func (s *Server) awaitOpening(w http.ResponseWriter, r *http.Request, c oidc.Claims) {
	if err := s.dbm.db.Directory().RecordOpsRequest(r.Context(), claimsAccount(c)); err != nil {
		s.events.Ops("warn", "ops_request_failed", err.Error(), map[string]any{"sub": c.Subject})
	}
	s.recordRefusal(r, entryOps, c, "waiting to be opened")
	_, _, err := s.dbm.store.Alerts().Open(r.Context(), repo.NewAlert{
		Kind: "ops_signin_pending", Fingerprint: opsPendingFingerprint(c.Subject), Severity: repo.SeverityWarn,
		SubjectType: "directory_account", SubjectID: c.Subject,
		Title:  c.PreferredUsername + " 等待開通運維後台",
		Detail: s.dbm.identity.toolName + "運維帳號 " + c.PreferredUsername + " 已通過二次驗證，還沒有控制台帳號。確認是自己人後，到「設定 → 管理員」的「待開通」裡點「建立並綁定」。",
	})
	if err != nil {
		s.events.Ops("warn", "alert_open_failed", err.Error(), nil)
	}
	s.refuseSignIn(w, r, entryOps, pendingMessage)
}

func opsPendingFingerprint(directoryID string) string { return "ops_signin_pending:" + directoryID }

// alertOpsRefusal: somebody the tool vouched for tried the operators' door
// and was turned away. Either a person who should have been linked, or a
// tool account being misused; a person looks at it either way.
func (s *Server) alertOpsRefusal(ctx context.Context, c oidc.Claims, why string) {
	_, _, err := s.dbm.store.Alerts().Open(ctx, repo.NewAlert{
		Kind: "ops_signin_refused", Fingerprint: "ops_signin_refused:" + c.Subject, Severity: repo.SeverityCrit,
		SubjectType: "directory_account", SubjectID: c.Subject,
		Title:  "運維入口拒絕了 " + c.PreferredUsername,
		Detail: "定製工具帳號 " + c.PreferredUsername + "（角色 " + c.Role + "）嘗試登入運維後台，被拒絕：" + why + "。",
	})
	if err != nil {
		s.events.Ops("warn", "alert_open_failed", err.Error(), nil)
	}
}

func (s *Server) refuseSignIn(w http.ResponseWriter, r *http.Request, entry, msg string) {
	target := "/customer/login"
	if entry == entryOps {
		target = opsSignInPath
	}
	http.Redirect(w, r, target+"?err="+url.QueryEscape(msg), http.StatusSeeOther)
}

var bounceTemplate = template.Must(template.New("bounce").Parse(`<!doctype html><html lang="zh-HK"><head><meta charset="utf-8">` +
	`<meta http-equiv="refresh" content="0;url={{.}}"><title>登入中</title></head><body><p>正在登入……<a href="{{.}}">如果沒有自動跳轉，請點這裡</a></p></body></html>`))

// bounce finishes a sign-in with a page of this site that moves on, rather
// than a redirect. The session cookie is SameSite=Strict, and a redirect at
// the end of a chain that began on the tool's site is still a cross-site
// navigation in some browsers, which would arrive without it.
func bounce(w http.ResponseWriter, to string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = bounceTemplate.Execute(w, to)
}

// oidcLogoutURL is where to send somebody who signed out of the customer
// entry: the tool's own sign-out, so the next visit does not sign them
// straight back in. Empty when there is no such page.
func (s *Server) oidcLogoutURL(r *http.Request, back string) string {
	if !s.oidcMode() {
		return ""
	}
	id := s.dbm.identity
	return id.provider.LogoutURL(r.Context(), id.base()+back)
}

// errManagedByTool refuses a change the custom tool owns in the oidc mode.
var errManagedByTool = errors.New("帳號由定製工具管理，請在定製工具後台修改")

// ssoTool is the tool's name in the oidc mode, empty otherwise.
func (s *Server) ssoTool() string {
	if !s.oidcMode() {
		return ""
	}
	return s.dbm.identity.toolName
}

// alertEmergencySignIn: the emergency account is for when the tool is down.
// Any use of it is something the others should know about at once.
func (s *Server) alertEmergencySignIn(r *http.Request, username string) {
	_, _, err := s.dbm.store.Alerts().Open(r.Context(), repo.NewAlert{
		Kind: "emergency_signin", Fingerprint: "emergency_signin:" + time.Now().UTC().Format(time.RFC3339),
		Severity: repo.SeverityCrit, SubjectType: "admin", SubjectID: username,
		Title:  "緊急帳號 " + username + " 已登入",
		Detail: "來源 " + s.clientKey(r) + "。緊急帳號只應在定製工具無法使用時登入，請確認是誰、為什麼。",
	})
	if err != nil {
		s.events.Ops("warn", "alert_open_failed", err.Error(), nil)
	}
}

// actionAdminLink binds a console account to the tool's ops account of the
// same person, which puts it on the whitelist of the ops entry.
func (s *Server) actionAdminLink(sess *session, r *http.Request) error {
	if sess.admin.Role != repo.RoleAdmin {
		return errors.New("只有 admin 角色可以綁定帳號")
	}
	target, err := s.dbm.store.Admins().ByUsername(r.Context(), formValue(r, "username"))
	if err != nil {
		return errors.New("找不到這個管理員")
	}
	dirID := formValue(r, "directory_id")
	if err := s.dbm.db.Directory().LinkAdmin(r.Context(), target.ID, dirID); err != nil {
		if errors.Is(err, dbstore.ErrNotPermitted) {
			return errors.New("只能綁定定製工具裡的運維帳號")
		}
		if errors.Is(err, repo.ErrDuplicate) {
			return errors.New("這個定製工具帳號已綁定到另一個管理員")
		}
		return err
	}
	action := "admin.link"
	if dirID == "" {
		action = "admin.unlink"
	} else {
		s.resolveOpsPending(r.Context(), dirID, sess.actor)
	}
	s.recordAccountEvent(r, sess.actor, action, "admin", target.ID, map[string]any{"username": target.Username, "directory_id": dirID})
	return nil
}

// actionAdminOpen creates a console account for an ops account on the
// waiting list and binds it, so its next sign-in comes in.
func (s *Server) actionAdminOpen(sess *session, r *http.Request) error {
	if sess.admin.Role != repo.RoleAdmin {
		return errors.New("只有 admin 角色可以開通帳號")
	}
	dirID, username, role := formValue(r, "directory_id"), formValue(r, "username"), formValue(r, "role")
	if role != repo.RoleAdmin && role != repo.RoleViewer {
		return errors.New("只能開通 admin 或 viewer 帳號")
	}
	id, err := s.dbm.db.Directory().OpenOpsAccount(r.Context(), dirID, username, role)
	switch {
	case errors.Is(err, dbstore.ErrNotPermitted):
		return errors.New("只能開通" + s.ssoTool() + "裡啟用中的運維帳號；剛建立的帳號要等下一次名單同步（最多 5 分鐘）")
	case errors.Is(err, repo.ErrDuplicate):
		return errors.New("這個用戶名已被使用，或這個帳號已經開通過")
	case err != nil:
		return err
	}
	s.resolveOpsPending(r.Context(), dirID, sess.actor)
	s.recordAccountEvent(r, sess.actor, "admin.open", "admin", id, map[string]any{
		"username": normalizeName(username), "role": role, "directory_id": dirID,
	})
	return nil
}

func (s *Server) resolveOpsPending(ctx context.Context, directoryID, by string) {
	if _, err := s.dbm.store.Alerts().Resolve(ctx, opsPendingFingerprint(directoryID), by); err != nil {
		s.events.Ops("warn", "alert_resolve_failed", err.Error(), nil)
	}
}

// suggestedUsername is the console user name offered for a tool account:
// the part of an e-mail address before the @.
func suggestedUsername(toolUsername string) string {
	name, _, _ := strings.Cut(normalizeName(toolUsername), "@")
	return name
}

func normalizeName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
