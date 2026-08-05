package authflow

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// TestCodeChallenge checks the transform against a value computed by hand,
// so a future refactor can't silently swap in the wrong hash or encoding.
func TestCodeChallenge(t *testing.T) {
	verifier := "example-verifier-value"
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	got := CodeChallenge(verifier)

	if got != want {
		t.Fatalf("CodeChallenge(%q) = %q, want %q", verifier, got, want)
	}
	if strings.ContainsAny(got, "+/=") {
		t.Fatalf("CodeChallenge(%q) = %q contains non-base64url characters", verifier, got)
	}
}

// TestNewCodexPKCE locks in Codex's hex encoding: 64 random bytes -> 128 hex
// chars for the verifier, 32 random bytes -> 64 hex chars for state.
func TestNewCodexPKCE(t *testing.T) {
	verifier, challenge, state, err := NewCodexPKCE()
	if err != nil {
		t.Fatalf("NewCodexPKCE() returned error: %v", err)
	}

	if len(verifier) != 128 {
		t.Errorf("verifier length = %d, want 128", len(verifier))
	}
	if decoded, decErr := hex.DecodeString(verifier); decErr != nil {
		t.Errorf("verifier is not valid hex: %v", decErr)
	} else if len(decoded) != 64 {
		t.Errorf("decoded verifier length = %d, want 64 bytes", len(decoded))
	}

	if len(state) != 64 {
		t.Errorf("state length = %d, want 64", len(state))
	}
	if decoded, decErr := hex.DecodeString(state); decErr != nil {
		t.Errorf("state is not valid hex: %v", decErr)
	} else if len(decoded) != 32 {
		t.Errorf("decoded state length = %d, want 32 bytes", len(decoded))
	}

	if want := CodeChallenge(verifier); challenge != want {
		t.Errorf("challenge = %q, want %q", challenge, want)
	}
}

// TestNewClaudePKCE locks in Claude's base64url encoding: 32 random bytes ->
// 43 base64url chars (no padding) for both the verifier and state.
func TestNewClaudePKCE(t *testing.T) {
	verifier, challenge, state, err := NewClaudePKCE()
	if err != nil {
		t.Fatalf("NewClaudePKCE() returned error: %v", err)
	}

	if len(verifier) != 43 {
		t.Errorf("verifier length = %d, want 43", len(verifier))
	}
	if strings.ContainsAny(verifier, "+/=") {
		t.Errorf("verifier %q contains non-base64url characters", verifier)
	}
	if decoded, decErr := base64.RawURLEncoding.DecodeString(verifier); decErr != nil {
		t.Errorf("verifier is not valid base64url: %v", decErr)
	} else if len(decoded) != 32 {
		t.Errorf("decoded verifier length = %d, want 32 bytes", len(decoded))
	}

	if len(state) != 43 {
		t.Errorf("state length = %d, want 43", len(state))
	}
	if strings.ContainsAny(state, "+/=") {
		t.Errorf("state %q contains non-base64url characters", state)
	}

	if want := CodeChallenge(verifier); challenge != want {
		t.Errorf("challenge = %q, want %q", challenge, want)
	}
}

// TestPKCERandomness makes sure back-to-back calls don't reuse bytes, which
// would be a catastrophic PKCE failure (predictable verifiers/state).
func TestPKCERandomness(t *testing.T) {
	v1, _, s1, err := NewCodexPKCE()
	if err != nil {
		t.Fatalf("NewCodexPKCE() returned error: %v", err)
	}
	v2, _, s2, err := NewCodexPKCE()
	if err != nil {
		t.Fatalf("NewCodexPKCE() returned error: %v", err)
	}
	if v1 == v2 {
		t.Error("NewCodexPKCE() produced identical verifiers on consecutive calls")
	}
	if s1 == s2 {
		t.Error("NewCodexPKCE() produced identical state on consecutive calls")
	}

	cv1, _, cs1, err := NewClaudePKCE()
	if err != nil {
		t.Fatalf("NewClaudePKCE() returned error: %v", err)
	}
	cv2, _, cs2, err := NewClaudePKCE()
	if err != nil {
		t.Fatalf("NewClaudePKCE() returned error: %v", err)
	}
	if cv1 == cv2 {
		t.Error("NewClaudePKCE() produced identical verifiers on consecutive calls")
	}
	if cs1 == cs2 {
		t.Error("NewClaudePKCE() produced identical state on consecutive calls")
	}
}
