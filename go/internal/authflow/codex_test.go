package authflow

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestCodexAuthorizeURL locks in the exact host, path, and query parameters
// the real Codex CLI sends. Admins are constructing this URL by hand to send
// to employees, so any drift here (missing param, wrong value) breaks login
// silently on OpenAI's side rather than with a clear client-side error.
func TestCodexAuthorizeURL(t *testing.T) {
	challenge := "test-challenge-value"
	state := "test-state-value"

	raw := CodexAuthorizeURL(challenge, state)

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("CodexAuthorizeURL returned unparseable URL: %v", err)
	}
	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want https", u.Scheme)
	}
	if u.Host != "auth.openai.com" {
		t.Errorf("host = %q, want auth.openai.com", u.Host)
	}
	if u.Path != "/oauth/authorize" {
		t.Errorf("path = %q, want /oauth/authorize", u.Path)
	}

	q := u.Query()
	want := map[string]string{
		"client_id":                  "app_EMoamEEZ73f0CkXaXp7hrann",
		"code_challenge":             challenge,
		"code_challenge_method":      "S256",
		"redirect_uri":               "http://localhost:1455/auth/callback",
		"response_type":              "code",
		"scope":                      "openid profile email offline_access",
		"state":                      state,
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
	}
	if len(q) != len(want) {
		t.Errorf("query has %d params, want %d (got %v)", len(q), len(want), q)
	}
	for key, val := range want {
		got := q.Get(key)
		if got != val {
			t.Errorf("query param %q = %q, want %q", key, got, val)
		}
	}
}

// fakeJWT builds a minimal unsigned JWT (header.payload.signature) with the
// given claims JSON as the payload, using the same base64.RawURLEncoding the
// real token issuer uses. The header and signature segments are dummy
// values since ExtractChatGPTAccountID never inspects them.
func fakeJWT(t *testing.T, claimsJSON string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(claimsJSON))
	return header + "." + payload + ".dummy-signature"
}

// TestExtractChatGPTAccountID_Namespaced covers the real-world token shape:
// account id lives under the "https://api.openai.com/auth" namespace claim,
// not at the top level.
func TestExtractChatGPTAccountID_Namespaced(t *testing.T) {
	token := fakeJWT(t, `{
		"sub": "user_123",
		"https://api.openai.com/auth": {
			"chatgpt_account_id": "acc_namespaced_456"
		}
	}`)

	got, err := ExtractChatGPTAccountID(token)
	if err != nil {
		t.Fatalf("ExtractChatGPTAccountID returned error: %v", err)
	}
	if got != "acc_namespaced_456" {
		t.Errorf("account id = %q, want acc_namespaced_456", got)
	}
}

// TestExtractChatGPTAccountID_TopLevel covers the fallback path in case a
// future token shape (or a different issuer config) puts the claim at the
// top level instead of namespaced.
func TestExtractChatGPTAccountID_TopLevel(t *testing.T) {
	token := fakeJWT(t, `{
		"sub": "user_123",
		"chatgpt_account_id": "acc_toplevel_789"
	}`)

	got, err := ExtractChatGPTAccountID(token)
	if err != nil {
		t.Fatalf("ExtractChatGPTAccountID returned error: %v", err)
	}
	if got != "acc_toplevel_789" {
		t.Errorf("account id = %q, want acc_toplevel_789", got)
	}
}

// TestExtractChatGPTAccountID_Missing ensures a clear error surfaces instead
// of silently returning an empty string, since an empty account_id would
// later produce an auth.json that Codex silently rejects.
func TestExtractChatGPTAccountID_Missing(t *testing.T) {
	token := fakeJWT(t, `{"sub": "user_123"}`)

	_, err := ExtractChatGPTAccountID(token)
	if err == nil {
		t.Fatal("ExtractChatGPTAccountID returned nil error for token with no account id")
	}
}

// TestExtractChatGPTAccountID_MalformedToken ensures garbage input produces
// an error rather than a panic.
func TestExtractChatGPTAccountID_MalformedToken(t *testing.T) {
	_, err := ExtractChatGPTAccountID("not-a-jwt")
	if err == nil {
		t.Fatal("ExtractChatGPTAccountID returned nil error for malformed token")
	}
}

// validTokens returns a CodexTokens whose IDToken carries a resolvable
// account id, for tests that need a fully happy-path input.
func validTokens(t *testing.T) *CodexTokens {
	t.Helper()
	idToken := fakeJWT(t, `{
		"https://api.openai.com/auth": {
			"chatgpt_account_id": "acc_build_test"
		}
	}`)
	return &CodexTokens{
		IDToken:      idToken,
		AccessToken:  "access-token-value",
		RefreshToken: "refresh-token-value",
	}
}

// TestBuildCodexAuthJSON_Format pins down the exact byte-level formatting
// Codex's auth.json parser silently requires: 2-space indent, exactly one
// space after each colon, a trailing newline, and no doubled spaces from a
// misconfigured encoder.
func TestBuildCodexAuthJSON_Format(t *testing.T) {
	tokens := validTokens(t)

	data, err := BuildCodexAuthJSON(tokens)
	if err != nil {
		t.Fatalf("BuildCodexAuthJSON returned error: %v", err)
	}

	s := string(data)

	if !strings.HasSuffix(s, "\n") {
		t.Error("output does not end with a trailing newline")
	}
	if !strings.Contains(s, "  \"auth_mode\": \"chatgpt\"") {
		t.Errorf("output missing 2-space-indented auth_mode line with single space after colon; got:\n%s", s)
	}
	if strings.Contains(s, "\":  ") {
		t.Errorf("output contains a doubled space after a colon; got:\n%s", s)
	}
	if data[0] == 0xEF {
		t.Error("output starts with a UTF-8 BOM")
	}
}

// TestBuildCodexAuthJSON_RoundTrip verifies the emitted JSON actually
// unmarshals back into the expected shape and values, including the null
// OPENAI_API_KEY that Codex checks for explicitly.
func TestBuildCodexAuthJSON_RoundTrip(t *testing.T) {
	tokens := validTokens(t)

	data, err := BuildCodexAuthJSON(tokens)
	if err != nil {
		t.Fatalf("BuildCodexAuthJSON returned error: %v", err)
	}

	var parsed struct {
		AuthMode     string          `json:"auth_mode"`
		OpenAIAPIKey json.RawMessage `json:"OPENAI_API_KEY"`
		Tokens       struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output does not parse as JSON: %v", err)
	}

	if parsed.AuthMode != "chatgpt" {
		t.Errorf("auth_mode = %q, want chatgpt", parsed.AuthMode)
	}
	if string(parsed.OpenAIAPIKey) != "null" {
		t.Errorf("OPENAI_API_KEY = %s, want null", parsed.OpenAIAPIKey)
	}
	if parsed.Tokens.IDToken != tokens.IDToken {
		t.Errorf("tokens.id_token mismatch")
	}
	if parsed.Tokens.AccessToken != tokens.AccessToken {
		t.Errorf("tokens.access_token = %q, want %q", parsed.Tokens.AccessToken, tokens.AccessToken)
	}
	if parsed.Tokens.RefreshToken != tokens.RefreshToken {
		t.Errorf("tokens.refresh_token = %q, want %q", parsed.Tokens.RefreshToken, tokens.RefreshToken)
	}
	if parsed.Tokens.AccountID != "acc_build_test" {
		t.Errorf("tokens.account_id = %q, want acc_build_test", parsed.Tokens.AccountID)
	}
	if !strings.HasSuffix(parsed.LastRefresh, "Z") {
		t.Errorf("last_refresh = %q, want a Z-suffixed UTC timestamp", parsed.LastRefresh)
	}
}

// TestBuildCodexAuthJSON_MissingAccountID ensures a token whose account id
// can't be resolved fails loudly instead of producing an auth.json with an
// empty account_id, which Codex would silently refuse to use.
func TestBuildCodexAuthJSON_MissingAccountID(t *testing.T) {
	idToken := fakeJWT(t, `{"sub": "user_123"}`)
	tokens := &CodexTokens{
		IDToken:      idToken,
		AccessToken:  "access-token-value",
		RefreshToken: "refresh-token-value",
	}

	_, err := BuildCodexAuthJSON(tokens)
	if err == nil {
		t.Fatal("BuildCodexAuthJSON returned nil error for token with no resolvable account id")
	}
}
