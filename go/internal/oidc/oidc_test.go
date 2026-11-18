package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testProvider is an OpenID provider with one RSA key, serving discovery and
// the key set; tokens are made by sign.
type testProvider struct {
	*httptest.Server
	key *rsa.PrivateKey
}

func newTestProvider(t *testing.T) *testProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tp := &testProvider{key: key}
	mux := http.NewServeMux()
	tp.Server = httptest.NewServer(mux)
	t.Cleanup(tp.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": tp.URL, "authorization_endpoint": tp.URL + "/authorize", "token_endpoint": tp.URL + "/token",
			"jwks_uri": tp.URL + "/jwks", "end_session_endpoint": tp.URL + "/logout",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	return tp
}

func seg(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (tp *testProvider) sign(t *testing.T, head, claims map[string]any) string {
	t.Helper()
	signed := seg(head) + "." + seg(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, tp.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (tp *testProvider) claims() map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss": tp.URL, "aud": "console", "sub": "00000000-0000-4000-8000-000000000001",
		"exp": now + 300, "iat": now, "auth_time": now - 30, "nonce": "n0nce", "role": "ops", "amr": []string{"pwd", "otp"},
		"preferred_username": "wei@example.com", "email": "Wei@Example.com",
	}
}

func TestVerifyAcceptsAGoodTokenAndNothingElse(t *testing.T) {
	tp := newTestProvider(t)
	p := &Provider{Issuer: tp.URL, ClientID: "console", ClientSecret: "s"}
	head := map[string]any{"alg": "RS256", "kid": "k1"}

	c, err := p.Verify(context.Background(), tp.sign(t, head, tp.claims()))
	if err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
	if !c.OpsSecondFactor() || c.Role != "ops" || c.Email != "wei@example.com" {
		t.Errorf("claims = %+v", c)
	}

	with := func(k string, v any) map[string]any {
		m := tp.claims()
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	good := tp.sign(t, head, tp.claims())
	parts := strings.Split(good, ".")
	for name, token := range map[string]string{
		"wrong issuer":      tp.sign(t, head, with("iss", "https://evil.example.com")),
		"wrong audience":    tp.sign(t, head, with("aud", "someone-else")),
		"expired":           tp.sign(t, head, with("exp", time.Now().Add(-time.Hour).Unix())),
		"no expiry":         tp.sign(t, head, with("exp", nil)),
		"issued in future":  tp.sign(t, head, with("iat", time.Now().Add(time.Hour).Unix())),
		"no subject":        tp.sign(t, head, with("sub", "")),
		"several audiences": tp.sign(t, head, with("aud", []string{"console", "other"})),
		"unknown key":       tp.sign(t, map[string]any{"alg": "RS256", "kid": "k9"}, tp.claims()),
		"alg none":          seg(map[string]any{"alg": "none", "kid": "k1"}) + "." + seg(tp.claims()) + ".",
		"alg HS256":         seg(map[string]any{"alg": "HS256", "kid": "k1"}) + "." + parts[1] + "." + parts[2],
		"claims swapped":    parts[0] + "." + seg(with("role", "admin")) + "." + parts[2],
		"not a JWT":         "abc",
		"signature mangled": parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte("nope")),
	} {
		if _, err := p.Verify(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: accepted or wrong error: %v", name, err)
		}
	}
}

func TestExchangeChecksTheNonceAndSendsTheVerifier(t *testing.T) {
	tp := newTestProvider(t)
	var form map[string]string
	var user, pass string
	tp.Config.Handler.(*http.ServeMux).HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = map[string]string{"code": r.PostFormValue("code"), "code_verifier": r.PostFormValue("code_verifier")}
		user, pass, _ = r.BasicAuth()
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": tp.sign(t, map[string]any{"alg": "RS256", "kid": "k1"}, tp.claims())})
	})
	p := &Provider{Issuer: tp.URL, ClientID: "console", ClientSecret: "s3cret"}
	flow := Flow{State: "s", Nonce: "n0nce", Verifier: "v"}
	if _, err := p.Exchange(context.Background(), flow, "the-code", "https://console/cb"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if form["code"] != "the-code" || form["code_verifier"] != "v" || user != "console" || pass != "s3cret" {
		t.Errorf("token request: %v %q %q", form, user, pass)
	}
	flow.Nonce = "another"
	if _, err := p.Exchange(context.Background(), flow, "the-code", "https://console/cb"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token for another sign-in was accepted: %v", err)
	}

	u, err := p.AuthURL(context.Background(), flow, "https://console/cb", 10*time.Minute)
	if err != nil || !strings.Contains(u, "code_challenge_method=S256") || !strings.Contains(u, "max_age=600") || strings.Contains(u, "code_verifier") {
		t.Errorf("auth URL = %s %v", u, err)
	}
}

func TestADiscoveryNamingAnotherIssuerIsRefused(t *testing.T) {
	tp := newTestProvider(t)
	p := &Provider{Issuer: tp.URL + "/other", ClientID: "console"}
	if _, err := p.AuthURL(context.Background(), Flow{}, "https://console/cb", 0); err == nil {
		t.Error("a provider that names another issuer was trusted")
	}
	if err := (&Provider{Issuer: "http://127.0.0.1:1"}).Reachable(context.Background()); err == nil {
		t.Error("an unreachable provider was reported reachable")
	}
}

func TestTheOpsEntryTakesTheAuthenticatorOrATextButNotAPasswordAlone(t *testing.T) {
	for name, c := range map[string]struct {
		amr  []string
		want bool
	}{
		"authenticator": {[]string{"pwd", "otp"}, true},
		"text message":  {[]string{"pwd", "sms"}, true},
		"password only": {[]string{"pwd"}, false},
		"no amr":        {nil, false},
	} {
		if got := (Claims{AMR: c.amr}).OpsSecondFactor(); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

func TestAuthenticatedWithinTrustsOnlyARecentPassword(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for name, c := range map[string]struct {
		at   int64
		want bool
	}{
		"just now":           {now.Unix() - 5, true},
		"nine minutes ago":   {now.Unix() - 9*60, true},
		"twelve minutes ago": {now.Unix() - 12*60, false},
		"no auth_time":       {0, false},
		"in the far future":  {now.Unix() + 3600, false},
	} {
		if got := (Claims{AuthTime: c.at}).AuthenticatedWithin(10*time.Minute, now); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}
