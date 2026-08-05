package authflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Claude OAuth constants. These were confirmed against the real
// claude.ai / platform.claude.com endpoints by trial — do not "clean up" or
// guess-correct any of these values without re-verifying against a live
// flow, since a wrong client_id or scope fails silently (redirect to a
// generic error page) rather than with a clear 4xx.
const (
	claudeClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeAuthorizeURL = "https://claude.ai/oauth/authorize"
	claudeTokenURL     = "https://platform.claude.com/v1/oauth/token"
	claudeRedirectURI  = "https://platform.claude.com/oauth/code/callback"
	claudeDefaultScope = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	// claudeUserAgent impersonates the axios version Claude Code's own CLI
	// uses to talk to the token endpoint. Go's default net/http User-Agent
	// (or an absent one) gets flagged by Claude's WAF as bot-like traffic and
	// rate-limited (the token endpoint replies with a rate_limit_error even
	// on the very first request), so this is load-bearing, not cosmetic.
	claudeUserAgent = "axios/1.13.6"

	// claudeRefreshFallbackTTL is used when the token endpoint's response
	// omits (or zeroes) refresh_token_expires_in, so we still write a usable
	// expiry into credentials.json instead of one that reads as "already
	// expired".
	claudeRefreshFallbackTTL = 30 * 24 * time.Hour
)

// ClaudeAuthorizeURL builds the URL an admin sends to an employee to start
// the Claude Code sign-in flow.
//
// This is built by hand with fmt.Sprintf instead of net/url.Values
// specifically because "code=true" must be the *first* query parameter.
// url.Values.Encode() always sorts keys alphabetically ("client_id" would
// land before "code"), which reorders this parameter and breaks the flow on
// Claude's side — confirmed against the real endpoint. Every value is
// individually percent-escaped with url.QueryEscape to stay safe even though
// the current values don't strictly need it.
func ClaudeAuthorizeURL(challenge, state string) string {
	return fmt.Sprintf(
		"%s?code=true&client_id=%s&response_type=code&redirect_uri=%s&scope=%s&code_challenge=%s&code_challenge_method=S256&state=%s",
		claudeAuthorizeURL,
		url.QueryEscape(claudeClientID),
		url.QueryEscape(claudeRedirectURI),
		url.QueryEscape(claudeDefaultScope),
		url.QueryEscape(challenge),
		url.QueryEscape(state),
	)
}

// ParsePastedClaudeCode splits the value a user pastes back after
// authorizing. Unlike Codex, Claude doesn't run a localhost redirect
// listener — it shows the authorization code directly on the page for the
// user to copy, sometimes as "code#state" and sometimes as just "code" (in
// which case the state we generated locally at authorize time is reused for
// the token exchange). Leading/trailing whitespace from the copy-paste is
// trimmed from the input and from the split-out code/state.
func ParsePastedClaudeCode(pasted, fallbackState string) (code, state string) {
	pasted = strings.TrimSpace(pasted)

	if idx := strings.Index(pasted, "#"); idx != -1 {
		code = strings.TrimSpace(pasted[:idx])
		state = strings.TrimSpace(pasted[idx+1:])
		return code, state
	}

	return pasted, fallbackState
}

// ClaudeTokens is the token endpoint's JSON response shape, decoded directly
// from the /v1/oauth/token body.
type ClaudeTokens struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	Scope                 string `json:"scope"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`

	// The endpoint also identifies who just signed in. Claude Code expects
	// this in ~/.claude.json, and without it the employee is asked to sign
	// in again even though their tokens are already on disk.
	Account      ClaudeAccount      `json:"account"`
	Organization ClaudeOrganization `json:"organization"`
}

// ClaudeAccount is the signed-in identity returned by the token endpoint.
type ClaudeAccount struct {
	UUID         string `json:"uuid"`
	EmailAddress string `json:"email_address"`
}

// ClaudeOrganization is the workspace the account belongs to.
type ClaudeOrganization struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// claudeExchangeRequest is the JSON body sent to the token endpoint. Unlike
// Codex (which takes a form-encoded body), Claude's token endpoint requires
// JSON — sending form-encoded data here gets rejected.
type claudeExchangeRequest struct {
	Code         string `json:"code"`
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier"`
	State        string `json:"state"`
}

// ClaudeExchange trades an authorization code for access/refresh tokens.
func ClaudeExchange(ctx context.Context, code, state, verifier string) (*ClaudeTokens, error) {
	reqBody, err := json.Marshal(claudeExchangeRequest{
		Code:         code,
		GrantType:    "authorization_code",
		ClientID:     claudeClientID,
		RedirectURI:  claudeRedirectURI,
		CodeVerifier: verifier,
		State:        state,
	})
	if err != nil {
		return nil, fmt.Errorf("authflow: marshal claude token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("authflow: build claude token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// See claudeUserAgent's doc comment: the default Go User-Agent gets
	// rate-limited by Claude's WAF, so we must impersonate axios here.
	req.Header.Set("User-Agent", claudeUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authflow: claude token request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("authflow: read claude token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authflow: claude token exchange failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var tokens ClaudeTokens
	if err := json.Unmarshal(respBody, &tokens); err != nil {
		return nil, fmt.Errorf("authflow: parse claude token response: %w (body: %s)", err, string(respBody))
	}

	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("authflow: claude token response missing access_token (body: %s)", string(respBody))
	}
	if tokens.RefreshToken == "" {
		return nil, fmt.Errorf("authflow: claude token response missing refresh_token (body: %s)", string(respBody))
	}

	return &tokens, nil
}

// claudeCredentials mirrors the on-disk shape Claude Code reads from
// ~/.claude/.credentials.json.
type claudeCredentials struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken"`
	ExpiresAt             int64    `json:"expiresAt"`
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt"`
	Scopes                []string `json:"scopes"`
	SubscriptionType      string   `json:"subscriptionType"`
	RateLimitTier         string   `json:"rateLimitTier"`
}

// BuildClaudeCredentialsJSON renders the exact bytes to write to
// ~/.claude/.credentials.json for the account that just completed the OAuth
// exchange.
func BuildClaudeCredentialsJSON(t *ClaudeTokens) ([]byte, error) {
	now := time.Now()
	nowMillis := now.UnixMilli()

	expiresAt := nowMillis + t.ExpiresIn*1000

	var refreshTokenExpiresAt int64
	if t.RefreshTokenExpiresIn > 0 {
		refreshTokenExpiresAt = nowMillis + t.RefreshTokenExpiresIn*1000
	} else {
		// The token endpoint doesn't always return refresh_token_expires_in.
		// Falling back to "now" would make Claude Code treat the refresh
		// token as already expired, so use a conservative 30-day estimate
		// instead.
		refreshTokenExpiresAt = now.Add(claudeRefreshFallbackTTL).UnixMilli()
	}

	scopeSource := t.Scope
	if strings.TrimSpace(scopeSource) == "" {
		scopeSource = claudeDefaultScope
	}
	scopes := strings.Fields(scopeSource)
	sort.Strings(scopes)

	creds := claudeCredentials{
		AccessToken:           t.AccessToken,
		RefreshToken:          t.RefreshToken,
		ExpiresAt:             expiresAt,
		RefreshTokenExpiresAt: refreshTokenExpiresAt,
		Scopes:                scopes,
		// The token endpoint's response never includes these two fields —
		// Claude Code itself populates them on its first real run against
		// the account. Writing anything else here (e.g. guessing "pro" or
		// "default") would just be stale/wrong until Claude Code overwrites
		// it anyway, so an empty string is the honest value.
		SubscriptionType: "",
		RateLimitTier:    "",
	}

	// json.Marshal (not MarshalIndent) is required: Claude Code's own
	// credential file is single-line compact JSON, and while its reader is
	// likely whitespace-tolerant, matching the exact on-disk shape it writes
	// itself avoids surprises from any tooling that diffs or rewrites the
	// file.
	body, err := json.Marshal(struct {
		ClaudeAiOauth claudeCredentials `json:"claudeAiOauth"`
	}{ClaudeAiOauth: creds})
	if err != nil {
		return nil, fmt.Errorf("authflow: marshal claude credentials: %w", err)
	}

	return body, nil
}

// claudeConfig is the part of ~/.claude.json that has to be present for
// Claude Code to consider itself signed in.
//
// The file holds much more than this in normal use -- project history, MCP
// servers, tips state -- but those are the machine's own accumulated state,
// not something to deliver. Only these two keys are written, and the agent
// merges them into any existing file rather than replacing it.
type claudeConfig struct {
	OAuthAccount           *claudeOAuthAccount `json:"oauthAccount,omitempty"`
	HasCompletedOnboarding bool                `json:"hasCompletedOnboarding"`
}

type claudeOAuthAccount struct {
	AccountUUID      string `json:"accountUuid"`
	EmailAddress     string `json:"emailAddress"`
	OrganizationUUID string `json:"organizationUuid"`
}

// BuildClaudeConfigJSON renders the ~/.claude.json fragment for the account
// that just signed in.
//
// Delivering .credentials.json alone is not enough: Claude Code reads its
// signed-in identity from ~/.claude.json, and with that missing it runs the
// onboarding wizard and asks the employee to log in -- which is exactly what
// this whole arrangement exists to avoid, since they do not have the password.
//
// The account block is omitted when the token endpoint did not return one, so
// the file still carries hasCompletedOnboarding rather than writing a
// half-filled identity.
func BuildClaudeConfigJSON(t *ClaudeTokens) ([]byte, error) {
	cfg := claudeConfig{HasCompletedOnboarding: true}
	if t.Account.UUID != "" || t.Account.EmailAddress != "" {
		cfg.OAuthAccount = &claudeOAuthAccount{
			AccountUUID:      t.Account.UUID,
			EmailAddress:     t.Account.EmailAddress,
			OrganizationUUID: t.Organization.UUID,
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode claude.json: %w", err)
	}
	return append(data, '\n'), nil
}

// HasAccountIdentity reports whether the token response identified the
// account. Callers warn when it did not, because the delivered
// ~/.claude.json will then be missing the identity Claude Code looks for.
func (t *ClaudeTokens) HasAccountIdentity() bool {
	return t.Account.UUID != "" && t.Account.EmailAddress != ""
}
