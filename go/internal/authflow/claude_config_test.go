package authflow

import (
	"encoding/json"
	"testing"
)

// Delivering .credentials.json alone leaves the employee at the onboarding
// wizard, being asked for a password they do not have. The identity in
// ~/.claude.json is what makes the delivered tokens usable.
func TestBuildClaudeConfigJSONCarriesTheIdentity(t *testing.T) {
	tokens := &ClaudeTokens{
		AccessToken:  "sk-ant-oat01-x",
		Account:      ClaudeAccount{UUID: "acc-uuid", EmailAddress: "work1@company.com"},
		Organization: ClaudeOrganization{UUID: "org-uuid", Name: "Company"},
	}

	data, err := BuildClaudeConfigJSON(tokens)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("not valid JSON: %v (%s)", err, data)
	}
	if got["hasCompletedOnboarding"] != true {
		t.Error("hasCompletedOnboarding must be true or the wizard runs")
	}
	acct, ok := got["oauthAccount"].(map[string]any)
	if !ok {
		t.Fatalf("oauthAccount missing: %s", data)
	}
	if acct["accountUuid"] != "acc-uuid" {
		t.Errorf("accountUuid = %v", acct["accountUuid"])
	}
	if acct["emailAddress"] != "work1@company.com" {
		t.Errorf("emailAddress = %v", acct["emailAddress"])
	}
	if acct["organizationUuid"] != "org-uuid" {
		t.Errorf("organizationUuid = %v", acct["organizationUuid"])
	}

	// The access token belongs in .credentials.json, not here.
	if string(data) != "" && contains(data, "sk-ant-oat01-x") {
		t.Error("claude.json must not carry the access token")
	}
}

// If the endpoint ever stops returning the account block, write the file
// without a half-filled identity rather than one full of empty strings.
func TestBuildClaudeConfigJSONOmitsAnEmptyIdentity(t *testing.T) {
	data, err := BuildClaudeConfigJSON(&ClaudeTokens{AccessToken: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["oauthAccount"]; present {
		t.Errorf("an empty identity should be omitted, got %s", data)
	}
	if got["hasCompletedOnboarding"] != true {
		t.Error("hasCompletedOnboarding should still be set")
	}
}

// The token endpoint returns the identity alongside the tokens; the response
// shape is what the CLI depends on, so pin it.
func TestClaudeTokensParsesAccountAndOrganization(t *testing.T) {
	body := `{
	  "token_type": "Bearer",
	  "access_token": "sk-ant-oat01-x",
	  "refresh_token": "sk-ant-ort01-y",
	  "expires_in": 28800,
	  "scope": "user:inference user:profile",
	  "organization": {"uuid": "org-uuid", "name": "Company"},
	  "account": {"uuid": "acc-uuid", "email_address": "work1@company.com"}
	}`

	var got ClaudeTokens
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if !got.HasAccountIdentity() {
		t.Fatal("the account should have been recognised")
	}
	if got.Account.UUID != "acc-uuid" || got.Account.EmailAddress != "work1@company.com" {
		t.Errorf("account = %+v", got.Account)
	}
	if got.Organization.UUID != "org-uuid" {
		t.Errorf("organization = %+v", got.Organization)
	}
}

func TestHasAccountIdentityNeedsBothFields(t *testing.T) {
	cases := map[string]struct {
		tokens ClaudeTokens
		want   bool
	}{
		"complete": {ClaudeTokens{Account: ClaudeAccount{UUID: "u", EmailAddress: "e"}}, true},
		"no uuid":  {ClaudeTokens{Account: ClaudeAccount{EmailAddress: "e"}}, false},
		"no email": {ClaudeTokens{Account: ClaudeAccount{UUID: "u"}}, false},
		"neither":  {ClaudeTokens{}, false},
	}
	for name, c := range cases {
		if got := c.tokens.HasAccountIdentity(); got != c.want {
			t.Errorf("%s: HasAccountIdentity() = %v, want %v", name, got, c.want)
		}
	}
}

func contains(haystack []byte, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		string(haystack) != "" && indexOf(string(haystack), needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
