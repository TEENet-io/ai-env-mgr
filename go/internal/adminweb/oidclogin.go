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
//     factor and is linked to a console account here -- the link is the
//     whitelist -- and asks the tool for the password again every time.
//
// A refusal says only that the account may not come in, never why; the
// reason goes to the audit trail, and at the ops entry to an alert.

const (
	entryCustomer = "customer"
	entryOps      = "ops"

	oidcStateCookie = "aem_oidc"
	oidcFlowTTL     = 10 * time.Minute
	refusedMessage  = "此帳號無權進入"
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
	// Operators prove themselves again at every sign-in: a tool session
	// left open on some desk must not open the console.
	prompt := ""
	if entry == entryOps {
		prompt = "login"
	}
	target, err := id.provider.AuthURL(r.Context(), flow, id.redirectURL, prompt)
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
	}
	username := ""
	if why == "" {
		var err error
		username, err = s.dbm.db.Directory().AdminForSignIn(r.Context(), c.Subject)
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
	}
	s.recordAccountEvent(r, sess.actor, action, "admin", target.ID, map[string]any{"username": target.Username, "directory_id": dirID})
	return nil
}
