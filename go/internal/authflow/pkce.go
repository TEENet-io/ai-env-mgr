// Package authflow generates PKCE (Proof Key for Code Exchange) material for
// the OAuth authorization-code flows used to sign in to Codex and Claude.
//
// Both providers implement the same RFC 7636 challenge derivation
// (challenge = base64url(sha256(verifier))), but they disagree on how the
// verifier itself is encoded before it's sent as a query parameter: Codex's
// backend expects the verifier hex-encoded, while Claude's expects it
// base64url-encoded. This was confirmed against both providers' real token
// endpoints — using the wrong encoding for either one causes the code
// exchange to fail even though the value still satisfies RFC 7636's
// [A-Za-z0-9-._~] charset. NewCodexPKCE and NewClaudePKCE exist as separate
// functions specifically to keep that difference explicit and hard to
// accidentally merge away.
package authflow

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// CodeChallenge derives the RFC 7636 S256 code challenge from a verifier:
// base64url(sha256(verifier)), without padding. This step is identical for
// both the Codex and Claude flows — only how the verifier itself was
// produced differs.
func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomHex returns n random bytes hex-encoded, giving a string of length 2n.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("authflow: generate random hex bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// randomBase64URL returns n random bytes base64url-encoded without padding.
func randomBase64URL(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("authflow: generate random base64url bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewCodexPKCE generates a verifier, challenge, and state for the Codex
// OAuth flow. Codex's token endpoint expects the verifier hex-encoded (64
// random bytes -> 128 hex chars), not base64url-encoded like the PKCE spec's
// typical example and like Claude's own endpoint. Getting this wrong
// produces a verifier that is still spec-legal but that Codex's server will
// reject during code exchange.
func NewCodexPKCE() (verifier, challenge, state string, err error) {
	verifier, err = randomHex(64)
	if err != nil {
		return "", "", "", fmt.Errorf("authflow: new codex verifier: %w", err)
	}
	state, err = randomHex(32)
	if err != nil {
		return "", "", "", fmt.Errorf("authflow: new codex state: %w", err)
	}
	return verifier, CodeChallenge(verifier), state, nil
}

// NewClaudePKCE generates a verifier, challenge, and state for the Claude
// OAuth flow. Claude's token endpoint expects the verifier base64url-encoded
// without padding (32 random bytes -> 43 chars), the RFC 7636 norm — unlike
// Codex, which requires hex.
func NewClaudePKCE() (verifier, challenge, state string, err error) {
	verifier, err = randomBase64URL(32)
	if err != nil {
		return "", "", "", fmt.Errorf("authflow: new claude verifier: %w", err)
	}
	state, err = randomBase64URL(32)
	if err != nil {
		return "", "", "", fmt.Errorf("authflow: new claude state: %w", err)
	}
	return verifier, CodeChallenge(verifier), state, nil
}
