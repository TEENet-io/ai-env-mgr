package authflow

import (
	"fmt"
	"net/url"
	"strings"
)

// CodeFromCallback pulls the authorisation code out of what an administrator
// pasted after signing in on an employee's behalf: usually the whole callback
// URL from the address bar, sometimes just the code.
//
// stateVerified reports whether the callback actually carried a state that
// matched. A bare code has none, so the caller can say the verification did
// not happen rather than implying it did.
//
// This lives here rather than in either front end so the CLI and the web
// console cannot drift apart: they are two ways of driving the same login, and
// a difference between them would be a difference nobody chose.
func CodeFromCallback(pasted, wantState string) (code string, stateVerified bool, err error) {
	pasted = strings.TrimSpace(pasted)
	if u, perr := url.Parse(pasted); perr == nil && u.Query().Get("code") != "" {
		q := u.Query()
		got := q.Get("state")
		if got != "" && got != wantState {
			return "", false, fmt.Errorf("that callback belongs to a different login attempt (state mismatch); start again")
		}
		return q.Get("code"), got != "", nil
	}

	if strings.Contains(pasted, "code=") {
		return "", false, fmt.Errorf("could not parse that URL; paste the whole address bar contents")
	}
	if pasted == "" {
		return "", false, fmt.Errorf("nothing was pasted")
	}
	return pasted, false, nil
}

// ClaudeCodeFromPaste reads what Claude shows after sign-in, which is a code
// and state joined by "#" rather than a callback URL. A pasted URL is accepted
// too, for the operator who reaches for the address bar out of habit.
//
// fallbackState is used when the pasted value carries none, matching what the
// CLI has always done.
func ClaudeCodeFromPaste(pasted, fallbackState string) (code, state string) {
	pasted = strings.TrimSpace(pasted)
	if u, err := url.Parse(pasted); err == nil && u.Query().Get("code") != "" {
		pasted = u.Query().Get("code")
	}
	return ParsePastedClaudeCode(pasted, fallbackState)
}
