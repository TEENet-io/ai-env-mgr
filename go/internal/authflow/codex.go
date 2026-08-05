package authflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// These constants were captured from a live, working Codex CLI login and
// verified end to end against OpenAI's real endpoints. They are not
// documented anywhere official, so don't "clean them up" or guess at
// alternate values without re-testing against a real login.
const (
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	codexRedirectURI  = "http://localhost:1455/auth/callback"
	codexScope        = "openid profile email offline_access"

	// codexUserAgent must match what the real Codex CLI sends. OpenAI's
	// token endpoint has been observed to behave differently (or reject the
	// exchange) for requests that don't look like they come from the CLI,
	// so this is not cosmetic.
	codexUserAgent = "codex-cli/0.91.0"
)

// CodexAuthorizeURL builds the URL an admin opens (or hands to an employee)
// to start the Codex OAuth login. Besides the standard OAuth/PKCE
// parameters, it includes two undocumented OpenAI-private parameters
// (id_token_add_organizations, codex_cli_simplified_flow) that were found
// necessary during real-world testing to get the same login flow the
// official Codex CLI uses; omitting them still "works" but produces a
// different (and for our purposes broken) authorization flow.
func CodexAuthorizeURL(challenge, state string) string {
	q := url.Values{
		"client_id":                  {codexClientID},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"redirect_uri":               {codexRedirectURI},
		"response_type":              {"code"},
		"scope":                      {codexScope},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return codexAuthorizeURL + "?" + q.Encode()
}

// CodexTokens holds the three tokens returned by a successful Codex code
// exchange. All three end up in auth.json, so all three are required.
type CodexTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// CodexExchange trades an authorization code for tokens at Codex's token
// endpoint. The request body is form-urlencoded, not JSON — OpenAI's OAuth
// token endpoint follows the RFC 6749 convention here, unlike its other
// (JSON) APIs, and sending JSON instead silently fails the exchange.
func CodexExchange(ctx context.Context, code, verifier string) (*CodexTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {codexRedirectURI},
		"client_id":     {codexClientID},
		"code_verifier": {verifier},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("authflow: build codex token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// The token endpoint has been observed to treat requests without this
	// exact User-Agent differently from the real Codex CLI's requests, so
	// this must be set even though it looks unnecessary for a plain OAuth
	// exchange.
	req.Header.Set("User-Agent", codexUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authflow: codex token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("authflow: read codex token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authflow: codex token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokens CodexTokens
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("authflow: parse codex token response: %w (body: %s)", err, string(body))
	}

	if tokens.IDToken == "" || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return nil, fmt.Errorf("authflow: codex token response missing one or more tokens: %s", string(body))
	}

	return &tokens, nil
}

// ExtractChatGPTAccountID pulls the ChatGPT account id out of a Codex id
// token (a JWT). The id token is not verified here — the exchange already
// happened over TLS directly with OpenAI, so signature verification would
// add complexity without adding security for this use case; we only need
// to read a claim out of it.
//
// The claim is namespaced under "https://api.openai.com/auth" in every
// token observed in practice, but a top-level chatgpt_account_id is also
// checked as a fallback in case OpenAI ever flattens the claim.
func ExtractChatGPTAccountID(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("authflow: id_token is not a JWT (expected 3 dot-separated parts, got %d)", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("authflow: decode id_token payload: %w", err)
	}

	var claims struct {
		OpenAIAuth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("authflow: parse id_token claims: %w", err)
	}

	if claims.OpenAIAuth.ChatGPTAccountID != "" {
		return claims.OpenAIAuth.ChatGPTAccountID, nil
	}
	if claims.ChatGPTAccountID != "" {
		return claims.ChatGPTAccountID, nil
	}

	return "", fmt.Errorf("authflow: id_token has no chatgpt_account_id claim (checked both the %q namespace and the top level)", "https://api.openai.com/auth")
}

// codexAuthFile mirrors the exact shape ~/.codex/auth.json must have.
// Field order matters for readability (and for diffing against real Codex
// output during debugging), which is why this uses a plain struct with
// explicit json tags rather than a map.
type codexAuthFile struct {
	AuthMode     string          `json:"auth_mode"`
	OpenAIAPIKey json.RawMessage `json:"OPENAI_API_KEY"`
	Tokens       codexAuthTokens `json:"tokens"`
	LastRefresh  string          `json:"last_refresh"`
}

type codexAuthTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// BuildCodexAuthJSON renders the contents of ~/.codex/auth.json for the
// given tokens. Codex's own parser has been observed to silently ignore (or
// refuse to use) files that don't match its exact formatting, so this
// mirrors the real Codex CLI's own output byte-for-byte rather than just
// producing "valid JSON": 2-space indent, one space after each colon (the
// json.Encoder default), a trailing newline, and no BOM.
//
// account_id is derived from the id_token rather than accepted as a
// separate parameter, since it must always agree with whatever ChatGPT
// account the id_token itself was issued for. An empty account_id is
// treated as a hard error because Codex requires all four token fields
// (id_token, access_token, refresh_token, account_id) to be non-empty; a
// file with a blank account_id would look valid but fail at CLI startup.
func BuildCodexAuthJSON(t *CodexTokens) ([]byte, error) {
	accountID, err := ExtractChatGPTAccountID(t.IDToken)
	if err != nil {
		return nil, fmt.Errorf("authflow: build codex auth.json: %w", err)
	}
	if accountID == "" {
		return nil, fmt.Errorf("authflow: build codex auth.json: resolved account_id is empty")
	}

	file := codexAuthFile{
		AuthMode:     "chatgpt",
		OpenAIAPIKey: json.RawMessage("null"),
		Tokens: codexAuthTokens{
			IDToken:      t.IDToken,
			AccessToken:  t.AccessToken,
			RefreshToken: t.RefreshToken,
			AccountID:    accountID,
		},
		// UTC with nanosecond precision and a literal Z suffix, matching
		// what the real Codex CLI writes on refresh.
		LastRefresh: time.Now().UTC().Format("2006-01-02T15:04:05.9999999Z"),
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// Codex's client_id and redirect_uri (and, in principle, JWT contents)
	// can contain characters like '&' that json.Marshal HTML-escapes by
	// default; escaping would produce a file byte-for-byte different from
	// what the real CLI writes, so it's disabled.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(file); err != nil {
		return nil, fmt.Errorf("authflow: encode codex auth.json: %w", err)
	}

	return buf.Bytes(), nil
}
