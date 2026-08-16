package main

import "testing"

// The administrator pastes whatever the browser left in the address bar. That
// is usually the full callback URL, and occasionally a callback from an
// abandoned earlier attempt. The parsing itself lives in authflow, shared with
// the web console; this covers the terminal wrapper around it.
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

	t.Run("bare code", func(t *testing.T) {
		got, err := codeFromCallback("THECODE", state)
		if err != nil {
			t.Fatal(err)
		}
		if got != "THECODE" {
			t.Errorf("code = %q, want THECODE", got)
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
