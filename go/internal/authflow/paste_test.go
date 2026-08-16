package authflow

import "testing"

// The administrator pastes whatever the browser left behind: usually the whole
// callback URL, sometimes just the code, occasionally one from an abandoned
// attempt. Both front ends call this, so its behaviour is the behaviour.
func TestCodeFromCallback(t *testing.T) {
	const state = "abc123state"
	const cb = "http://localhost:1455/auth/callback?"

	t.Run("full callback URL", func(t *testing.T) {
		code, verified, err := CodeFromCallback(cb+"code=THECODE&state="+state, state)
		if err != nil || code != "THECODE" {
			t.Fatalf("code = %q, err = %v", code, err)
		}
		if !verified {
			t.Error("a matching state was not reported as verified")
		}
	})

	t.Run("URL-encoded code is decoded", func(t *testing.T) {
		code, _, err := CodeFromCallback(cb+"code=ab%2Fcd&state="+state, state)
		if err != nil || code != "ab/cd" {
			t.Fatalf("code = %q, err = %v", code, err)
		}
	})

	// Accepted, but the caller is told nothing was verified so it can say so
	// rather than implying a check happened.
	t.Run("bare code is accepted and reported unverified", func(t *testing.T) {
		code, verified, err := CodeFromCallback("THECODE", state)
		if err != nil || code != "THECODE" {
			t.Fatalf("code = %q, err = %v", code, err)
		}
		if verified {
			t.Error("a bare code was reported as state-verified")
		}
	})

	t.Run("state mismatch is refused", func(t *testing.T) {
		if _, _, err := CodeFromCallback(cb+"code=THECODE&state=someoneelse", state); err == nil {
			t.Fatal("a callback from another attempt was accepted")
		}
	})

	// A fragment that url.Parse cannot pull a code out of, but that plainly
	// came from a URL, is a half-finished paste rather than a code.
	t.Run("a fragment carrying code= is refused", func(t *testing.T) {
		if _, _, err := CodeFromCallback("code=THECODE", state); err == nil {
			t.Fatal("a half-pasted URL was accepted as a bare code")
		}
	})

	// A URL whose query parses but carries no state is accepted, unverified --
	// this is what the CLI has always done.
	t.Run("callback without state is accepted, unverified", func(t *testing.T) {
		code, verified, err := CodeFromCallback(cb+"code=THECODE", state)
		if err != nil || code != "THECODE" {
			t.Fatalf("code = %q, err = %v", code, err)
		}
		if verified {
			t.Error("an absent state was reported as verified")
		}
	})

	t.Run("nothing pasted", func(t *testing.T) {
		if _, _, err := CodeFromCallback("   ", state); err == nil {
			t.Fatal("an empty paste was accepted")
		}
	})
}

// Claude shows "code#state" instead of redirecting, so its paste needs
// splitting rather than URL parsing -- but a pasted URL still works.
func TestClaudeCodeFromPaste(t *testing.T) {
	const state = "abc123state"

	code, got := ClaudeCodeFromPaste("THECODE#"+state, "fallback")
	if code != "THECODE" || got != state {
		t.Fatalf("code = %q, state = %q", code, got)
	}

	// No "#": the caller's own state stands in, as the CLI has always done.
	code, got = ClaudeCodeFromPaste("THECODE", state)
	if code != "THECODE" || got != state {
		t.Fatalf("code = %q, state = %q", code, got)
	}

	code, got = ClaudeCodeFromPaste("https://claude.ai/cb?code=THECODE", state)
	if code != "THECODE" || got != state {
		t.Fatalf("a pasted URL gave code = %q, state = %q", code, got)
	}
}
