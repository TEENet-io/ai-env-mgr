package main

import "testing"

// The administrator pastes whatever the browser left in the address bar. That
// is usually the full callback URL, and occasionally a callback from an
// abandoned earlier attempt. Anything without a matching state is refused:
// the state is the only thing tying a pasted code to this login.
func TestCodeFromCallback(t *testing.T) {
	const state = "abc123state"

	t.Run("full callback URL", func(t *testing.T) {
		url := "http://localhost:1455/auth/callback?code=THECODE&state=" + state
		got, err := codeFromCallback(url, state)
		if err != nil {
			t.Fatal(err)
		}
		if got != "THECODE" {
			t.Errorf("code = %q, want THECODE", got)
		}
	})

	t.Run("URL-encoded code is decoded", func(t *testing.T) {
		url := "http://localhost:1455/auth/callback?code=ab%2Fcd&state=" + state
		got, err := codeFromCallback(url, state)
		if err != nil {
			t.Fatal(err)
		}
		if got != "ab/cd" {
			t.Errorf("code = %q, want ab/cd", got)
		}
	})

	// A bare code carries no state, so nothing says it came from this login
	// rather than from a URL somebody talked the operator into pasting. The
	// exchange would likely fail on the PKCE verifier anyway; refusing here
	// says why, and does not depend on that.
	t.Run("bare code is refused", func(t *testing.T) {
		if _, err := codeFromCallback("THECODE", state); err == nil {
			t.Fatal("a bare code was accepted with no state to verify")
		}
	})

	t.Run("callback with no state is refused", func(t *testing.T) {
		if _, err := codeFromCallback("http://localhost:1455/auth/callback?code=THECODE", state); err == nil {
			t.Fatal("a callback carrying no state was accepted")
		}
	})

	// An empty expected state must not compare equal to an absent one.
	t.Run("empty expected state is refused", func(t *testing.T) {
		if _, err := codeFromCallback("http://localhost:1455/auth/callback?code=THECODE", ""); err == nil {
			t.Fatal("an empty expected state matched an absent one")
		}
	})

	// A stale callback from an earlier attempt carries a different state. Its
	// code would fail the exchange anyway, but failing here says why.
	t.Run("state mismatch is refused", func(t *testing.T) {
		url := "http://localhost:1455/auth/callback?code=THECODE&state=someoldstate"
		if _, err := codeFromCallback(url, state); err == nil {
			t.Error("a callback from a different login should be refused")
		}
	})

	// Query order varies between providers and redirects.
	t.Run("state before code", func(t *testing.T) {
		url := "http://localhost:1455/auth/callback?state=" + state + "&code=THECODE"
		got, err := codeFromCallback(url, state)
		if err != nil {
			t.Fatal(err)
		}
		if got != "THECODE" {
			t.Errorf("code = %q, want THECODE", got)
		}
	})
}
