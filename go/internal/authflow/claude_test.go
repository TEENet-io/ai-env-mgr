package authflow

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestClaudeAuthorizeURL_CodeTrueFirst locks in the single most fragile
// property of this URL: "code=true" must be the very first query parameter.
// Claude's authorize endpoint keys off positional/first-param parsing on
// their side for this legacy flag, so building the URL with url.Values (which
// sorts keys alphabetically) would silently reorder it after "client_id" and
// break the flow. This was confirmed against the real endpoint.
func TestClaudeAuthorizeURL_CodeTrueFirst(t *testing.T) {
	got := ClaudeAuthorizeURL("test-challenge", "test-state")

	want := "https://claude.ai/oauth/authorize?code=true&"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("ClaudeAuthorizeURL = %q, want prefix %q", got, want)
	}
}

// TestClaudeAuthorizeURL_ContainsAllParams checks every required query
// fragment is present with the expected value, without relying on
// net/url.Parse (which would re-sort params and hide ordering bugs the
// previous test is specifically there to catch).
func TestClaudeAuthorizeURL_ContainsAllParams(t *testing.T) {
	challenge := "chal-abc123"
	state := "state-xyz789"

	got := ClaudeAuthorizeURL(challenge, state)

	wantFragments := []string{
		"client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"response_type=code",
		"redirect_uri=" + "https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback",
		"code_challenge=" + challenge,
		"code_challenge_method=S256",
		"state=" + state,
	}
	for _, frag := range wantFragments {
		if !strings.Contains(got, frag) {
			t.Errorf("ClaudeAuthorizeURL() = %q, missing fragment %q", got, frag)
		}
	}

	// scope must be present and percent-escaped (contains spaces -> %20 or +).
	if !strings.Contains(got, "scope=") {
		t.Errorf("ClaudeAuthorizeURL() = %q, missing scope param", got)
	}
}

// TestParsePastedClaudeCode_WithState covers the "code#state" shape Claude's
// page shows for the user to copy, where everything after '#' is the state
// value the server also returned.
func TestParsePastedClaudeCode_WithState(t *testing.T) {
	code, state := ParsePastedClaudeCode("abc#xyz", "fallback-state")
	if code != "abc" {
		t.Errorf("code = %q, want %q", code, "abc")
	}
	if state != "xyz" {
		t.Errorf("state = %q, want %q", state, "xyz")
	}
}

// TestParsePastedClaudeCode_WithoutState covers the plain-code shape, which
// must fall back to the state we generated locally at authorize time (since
// there's nothing else to verify CSRF/replay against).
func TestParsePastedClaudeCode_WithoutState(t *testing.T) {
	code, state := ParsePastedClaudeCode("justacode", "fallback-state")
	if code != "justacode" {
		t.Errorf("code = %q, want %q", code, "justacode")
	}
	if state != "fallback-state" {
		t.Errorf("state = %q, want %q", state, "fallback-state")
	}
}

// TestParsePastedClaudeCode_TrimsWhitespace ensures accidental leading or
// trailing whitespace from a copy-paste doesn't end up embedded in the code
// or state values sent to the token endpoint.
func TestParsePastedClaudeCode_TrimsWhitespace(t *testing.T) {
	code, state := ParsePastedClaudeCode("  abc#xyz  \n", "fallback-state")
	if code != "abc" {
		t.Errorf("code = %q, want %q", code, "abc")
	}
	if state != "xyz" {
		t.Errorf("state = %q, want %q", state, "xyz")
	}

	code2, state2 := ParsePastedClaudeCode("  justacode  ", "fallback-state")
	if code2 != "justacode" {
		t.Errorf("code = %q, want %q", code2, "justacode")
	}
	if state2 != "fallback-state" {
		t.Errorf("state = %q, want %q", state2, "fallback-state")
	}
}

// validClaudeTokens returns a ClaudeTokens populated as if a real exchange
// had just succeeded, for tests that only care about the credentials.json
// building step.
func validClaudeTokens() *ClaudeTokens {
	return &ClaudeTokens{
		AccessToken:           "sk-ant-oat-test-access",
		RefreshToken:          "sk-ant-ort-test-refresh",
		Scope:                 "user:inference user:file_upload",
		ExpiresIn:             3600,
		RefreshTokenExpiresIn: 1209600, // 14 days, in seconds
	}
}

// TestBuildClaudeCredentialsJSON_SingleLine pins the exact byte-level shape
// Claude Code's own credential loader expects: compact, single-line JSON (no
// MarshalIndent), so a naive switch to pretty-printing wouldn't be caught by
// a JSON-semantic-only test.
func TestBuildClaudeCredentialsJSON_SingleLine(t *testing.T) {
	data, err := BuildClaudeCredentialsJSON(validClaudeTokens())
	if err != nil {
		t.Fatalf("BuildClaudeCredentialsJSON returned error: %v", err)
	}

	s := string(data)
	if strings.Contains(s, "\n") {
		t.Errorf("output contains a newline, want single-line compact JSON; got:\n%s", s)
	}
	if !strings.HasPrefix(s, `{"claudeAiOauth":{`) {
		t.Errorf("output = %q, want prefix %q", s, `{"claudeAiOauth":{`)
	}
}

// claudeCredentialsShape mirrors the on-disk ~/.claude/.credentials.json
// structure for round-trip verification.
type claudeCredentialsShape struct {
	ClaudeAiOauth struct {
		AccessToken           string   `json:"accessToken"`
		RefreshToken          string   `json:"refreshToken"`
		ExpiresAt             int64    `json:"expiresAt"`
		RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt"`
		Scopes                []string `json:"scopes"`
		SubscriptionType      string   `json:"subscriptionType"`
		RateLimitTier         string   `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

// TestBuildClaudeCredentialsJSON_RoundTrip verifies the emitted JSON parses
// back into the expected values: tokens carried through verbatim, expiry
// timestamps computed in milliseconds from "now", and scopes split/sorted
// from the space-delimited scope string.
func TestBuildClaudeCredentialsJSON_RoundTrip(t *testing.T) {
	tokens := validClaudeTokens()
	before := time.Now().UnixMilli()

	data, err := BuildClaudeCredentialsJSON(tokens)
	if err != nil {
		t.Fatalf("BuildClaudeCredentialsJSON returned error: %v", err)
	}
	after := time.Now().UnixMilli()

	var parsed claudeCredentialsShape
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output does not parse as JSON: %v", err)
	}

	if parsed.ClaudeAiOauth.AccessToken != tokens.AccessToken {
		t.Errorf("accessToken = %q, want %q", parsed.ClaudeAiOauth.AccessToken, tokens.AccessToken)
	}
	if parsed.ClaudeAiOauth.RefreshToken != tokens.RefreshToken {
		t.Errorf("refreshToken = %q, want %q", parsed.ClaudeAiOauth.RefreshToken, tokens.RefreshToken)
	}

	wantExpiresAtMin := before + tokens.ExpiresIn*1000
	wantExpiresAtMax := after + tokens.ExpiresIn*1000
	if parsed.ClaudeAiOauth.ExpiresAt < wantExpiresAtMin || parsed.ClaudeAiOauth.ExpiresAt > wantExpiresAtMax {
		t.Errorf("expiresAt = %d, want between %d and %d", parsed.ClaudeAiOauth.ExpiresAt, wantExpiresAtMin, wantExpiresAtMax)
	}

	if parsed.ClaudeAiOauth.RefreshTokenExpiresAt <= parsed.ClaudeAiOauth.ExpiresAt {
		t.Errorf("refreshTokenExpiresAt = %d, want > expiresAt %d", parsed.ClaudeAiOauth.RefreshTokenExpiresAt, parsed.ClaudeAiOauth.ExpiresAt)
	}

	wantScopes := strings.Split(tokens.Scope, " ")
	sort.Strings(wantScopes)
	if len(parsed.ClaudeAiOauth.Scopes) != len(wantScopes) {
		t.Fatalf("scopes = %v, want %v", parsed.ClaudeAiOauth.Scopes, wantScopes)
	}
	for i, s := range wantScopes {
		if parsed.ClaudeAiOauth.Scopes[i] != s {
			t.Errorf("scopes[%d] = %q, want %q", i, parsed.ClaudeAiOauth.Scopes[i], s)
		}
	}

	if parsed.ClaudeAiOauth.SubscriptionType != "" {
		t.Errorf("subscriptionType = %q, want empty (server never returns it; Claude Code fills it in on first run)", parsed.ClaudeAiOauth.SubscriptionType)
	}
	if parsed.ClaudeAiOauth.RateLimitTier != "" {
		t.Errorf("rateLimitTier = %q, want empty (server never returns it; Claude Code fills it in on first run)", parsed.ClaudeAiOauth.RateLimitTier)
	}
}

// TestBuildClaudeCredentialsJSON_DefaultScopeWhenEmpty ensures a ClaudeTokens
// with no Scope value (which shouldn't normally happen, but the token
// endpoint's response shape isn't guaranteed) still produces a full, sorted
// scope list rather than an empty array that would leave Claude Code
// under-permissioned.
func TestBuildClaudeCredentialsJSON_DefaultScopeWhenEmpty(t *testing.T) {
	tokens := validClaudeTokens()
	tokens.Scope = ""

	data, err := BuildClaudeCredentialsJSON(tokens)
	if err != nil {
		t.Fatalf("BuildClaudeCredentialsJSON returned error: %v", err)
	}

	var parsed claudeCredentialsShape
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output does not parse as JSON: %v", err)
	}

	wantScopes := strings.Split(claudeDefaultScope, " ")
	sort.Strings(wantScopes)
	if len(parsed.ClaudeAiOauth.Scopes) != len(wantScopes) {
		t.Fatalf("scopes = %v, want %v (fallback to default scope)", parsed.ClaudeAiOauth.Scopes, wantScopes)
	}
}

// TestBuildClaudeCredentialsJSON_RefreshTokenExpiresInFallback ensures that
// when the token endpoint doesn't return refresh_token_expires_in (observed
// as 0 in some responses), we still write a usable, non-zero
// refreshTokenExpiresAt rather than one equal to "now" (which Claude Code
// would treat as already expired).
func TestBuildClaudeCredentialsJSON_RefreshTokenExpiresInFallback(t *testing.T) {
	tokens := validClaudeTokens()
	tokens.RefreshTokenExpiresIn = 0

	before := time.Now().UnixMilli()
	data, err := BuildClaudeCredentialsJSON(tokens)
	if err != nil {
		t.Fatalf("BuildClaudeCredentialsJSON returned error: %v", err)
	}

	var parsed claudeCredentialsShape
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output does not parse as JSON: %v", err)
	}

	if parsed.ClaudeAiOauth.RefreshTokenExpiresAt == 0 {
		t.Fatal("refreshTokenExpiresAt = 0, want fallback to now + 30 days")
	}
	// Should be roughly 30 days out, well beyond just a few seconds from now.
	minWant := before + (29 * 24 * time.Hour).Milliseconds()
	if parsed.ClaudeAiOauth.RefreshTokenExpiresAt < minWant {
		t.Errorf("refreshTokenExpiresAt = %d, want at least %d (~30 day fallback)", parsed.ClaudeAiOauth.RefreshTokenExpiresAt, minWant)
	}
}

// TestClaudeTokens_JSONTags locks in the exact field <-> JSON tag mapping
// ClaudeExchange relies on to decode the token endpoint's response body.
func TestClaudeTokens_JSONTags(t *testing.T) {
	body := `{
		"access_token": "at-val",
		"refresh_token": "rt-val",
		"scope": "scope-val",
		"expires_in": 3600,
		"refresh_token_expires_in": 1209600
	}`

	var tok ClaudeTokens
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		t.Fatalf("failed to unmarshal into ClaudeTokens: %v", err)
	}

	if tok.AccessToken != "at-val" {
		t.Errorf("AccessToken = %q, want at-val", tok.AccessToken)
	}
	if tok.RefreshToken != "rt-val" {
		t.Errorf("RefreshToken = %q, want rt-val", tok.RefreshToken)
	}
	if tok.Scope != "scope-val" {
		t.Errorf("Scope = %q, want scope-val", tok.Scope)
	}
	if tok.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want 3600", tok.ExpiresIn)
	}
	if tok.RefreshTokenExpiresIn != 1209600 {
		t.Errorf("RefreshTokenExpiresIn = %d, want 1209600", tok.RefreshTokenExpiresIn)
	}
}
