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
		return fmt.Errorf("paste the callback URL from the browser's address bar")
	}
	code, err := codeFromCallback(pasted, p.state)
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

// codeFromCallback pulls the authorisation code out of the pasted callback,
// checking that it belongs to the flow this session started.
func codeFromCallback(pasted, wantState string) (string, error) {
	pasted = strings.TrimSpace(pasted)
	if u, err := url.Parse(pasted); err == nil && u.Query().Get("code") != "" {
		q := u.Query()
		if got := q.Get("state"); got != "" && got != wantState {
			return "", fmt.Errorf("that callback belongs to a different sign-in attempt (state mismatch); start again")
		}
		return q.Get("code"), nil
	}
	if strings.Contains(pasted, "code=") {
		return "", fmt.Errorf("could not parse that URL; paste the whole address bar contents")
	}
	// A bare code carries no state to verify against, so refuse it here rather
	// than accepting a code that might belong to another flow. The address bar
	// always has the full URL.
	return "", fmt.Errorf("paste the whole callback URL, not just the code, so the state can be checked")
}
