package adminweb

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/authflow"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// pendingLogin holds one in-flight employee sign-in.
//
// The PKCE verifier and state never leave this process: they live in the
// session, so a second browser -- or anyone who guesses the URL -- cannot
// complete a flow somebody else started.
type pendingLogin struct {
	user     string
	tool     string // "codex" or "claude"
	verifier string
	state    string
	authURL  string // shown to the operator to open in a browser
	started  time.Time
}

const employeeLoginTTL = 15 * time.Minute

// actionEmployeeLoginStart builds the authorisation URL the administrator
// opens in a browser while signed in as the employee.
func (s *Server) actionEmployeeLoginStart(sess *session, r *http.Request) error {
	user := formValue(r, "windowsUser")
	tool := formValue(r, "tool")
	if user == "" {
		return fmt.Errorf("choose an employee")
	}
	if tool != "codex" && tool != "claude" {
		return fmt.Errorf("choose Codex or Claude")
	}

	var challenge, state, verifier, authURL string
	var err error
	switch tool {
	case "codex":
		verifier, challenge, state, err = authflow.NewCodexPKCE()
		if err == nil {
			authURL = authflow.CodexAuthorizeURL(challenge, state)
		}
	case "claude":
		verifier, challenge, state, err = authflow.NewClaudePKCE()
		if err == nil {
			authURL = authflow.ClaudeAuthorizeURL(challenge, state)
		}
	}
	if err != nil {
		return err
	}

	s.pendingMu.Lock()
	s.pending[sess.csrf] = &pendingLogin{
		user: user, tool: tool, verifier: verifier, state: state,
		authURL: authURL, started: time.Now(),
	}
	s.pendingMu.Unlock()
	return nil
}

// actionEmployeeLoginFinish exchanges the pasted callback for tokens and
// publishes them for the employee's machines to collect.
func (s *Server) actionEmployeeLoginFinish(sess *session, r *http.Request) error {
	s.pendingMu.Lock()
	p := s.pending[sess.csrf]
	s.pendingMu.Unlock()
	if p == nil {
		return fmt.Errorf("no sign-in is in progress; start one first")
	}
	if time.Since(p.started) > employeeLoginTTL {
		s.clearPending(sess)
		return fmt.Errorf("that sign-in took too long and expired; start again")
	}

	pasted := formValue(r, "callback")
	if pasted == "" {
		return fmt.Errorf("paste what the browser showed after signing in")
	}
	var err error
	var code string
	switch p.tool {
	case "codex":
		code, err = codexCodeFromCallback(pasted, p.state)
	case "claude":
		code, err = claudeCodeFromPaste(pasted, p.state)
	}
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var set model.CredentialSet
	switch p.tool {
	case "codex":
		tokens, err := authflow.CodexExchange(ctx, code, p.verifier)
		if err != nil {
			return err
		}
		authJSON, err := authflow.BuildCodexAuthJSON(tokens)
		if err != nil {
			return err
		}
		set = model.CredentialSet{model.PathCodexAuth: authJSON}
	case "claude":
		tokens, err := authflow.ClaudeExchange(ctx, code, p.state, p.verifier)
		if err != nil {
			return err
		}
		credsJSON, err := authflow.BuildClaudeCredentialsJSON(tokens)
		if err != nil {
			return err
		}
		set = model.CredentialSet{model.PathClaudeCreds: credsJSON}
	}

	if err := sess.mgr.PublishCredentials(p.user, set); err != nil {
		return err
	}
	s.clearPending(sess)
	// The employee's tokens themselves are never logged -- only that a sign-in
	// happened, for whom, and by which console session.
	logAudit(s.clientKey(r), "published %s credentials for %s", p.tool, p.user)
	return nil
}

func (s *Server) clearPending(sess *session) {
	s.pendingMu.Lock()
	delete(s.pending, sess.csrf)
	s.pendingMu.Unlock()
}

// codexCodeFromCallback pulls the authorisation code out of the callback URL
// Codex redirects to.
//
// The state must match unconditionally. Accepting a callback that simply
// carries no state -- which an earlier version did -- means accepting a code
// this session never asked for: an operator who pasted a crafted URL would
// publish the tokens of somebody else's account as the employee's credentials.
// PKCE already makes that hard, since a code issued for another challenge will
// not exchange against our verifier, but the check is the part that says so
// rather than relying on it.
func codexCodeFromCallback(pasted, wantState string) (string, error) {
	// Guard the comparison itself: with an empty wantState, a callback that
	// carries no state would compare equal and pass. That should never happen
	// -- a flow always stores one -- so treat it as a broken flow, not a match.
	if wantState == "" {
		return "", fmt.Errorf("this sign-in is missing its state; start again")
	}
	pasted = strings.TrimSpace(pasted)
	u, err := url.Parse(pasted)
	if err != nil || u.Query().Get("code") == "" {
		return "", fmt.Errorf("paste the whole callback URL from the address bar, starting with http://localhost:1455/")
	}
	if u.Query().Get("state") != wantState {
		return "", fmt.Errorf("that callback belongs to a different sign-in attempt (state mismatch); start again")
	}
	return u.Query().Get("code"), nil
}

// claudeCodeFromPaste reads what Claude displays after sign-in, which is a
// code and state joined by "#" rather than a callback URL. A pasted URL is
// tolerated for the operator who reaches for the address bar out of habit.
func claudeCodeFromPaste(pasted, wantState string) (string, error) {
	if wantState == "" {
		return "", fmt.Errorf("this sign-in is missing its state; start again")
	}
	pasted = strings.TrimSpace(pasted)
	if u, err := url.Parse(pasted); err == nil && u.Query().Get("code") != "" {
		if u.Query().Get("state") != wantState {
			return "", fmt.Errorf("that callback belongs to a different sign-in attempt (state mismatch); start again")
		}
		return u.Query().Get("code"), nil
	}
	code, state := authflow.ParsePastedClaudeCode(pasted, "")
	if code == "" {
		return "", fmt.Errorf("paste the code Claude showed you")
	}
	// Same reasoning as above: no state means nothing ties this code to the
	// flow this session started, so ask for the whole value rather than
	// guessing that it is ours.
	if state == "" {
		return "", fmt.Errorf("paste the whole value Claude showed, including the part after the # -- it is what ties the code to this sign-in")
	}
	if state != wantState {
		return "", fmt.Errorf("that code belongs to a different sign-in attempt (state mismatch); start again")
	}
	return code, nil
}
