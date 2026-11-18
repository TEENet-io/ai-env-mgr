// Package oidc signs people in through the customer's custom tool, which is
// an OpenID Connect provider (AIC-206).
//
// It is the relying party's half of the authorization code flow with PKCE,
// and nothing more: discovery, the redirect, the code exchange, and the
// checks on the ID token -- signature, issuer, audience, lifetime, nonce.
// It is written against the standard library rather than a general OIDC
// package because the console only ever talks to one provider of a known
// shape, and every check it relies on is here to read.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Provider is one OpenID Connect provider and this console's client there.
type Provider struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	Now          func() time.Time

	mu        sync.Mutex
	meta      *metadata
	keys      map[string]crypto.PublicKey
	keysAt    time.Time
	metaFetch time.Time
}

type metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

// Claims are what the console reads from an ID token (docs: 账号打通改造计划
// 附录 A).
type Claims struct {
	Issuer            string   `json:"iss"`
	Subject           string   `json:"sub"`
	Audience          audience `json:"aud"`
	AuthorizedParty   string   `json:"azp"`
	Expiry            int64    `json:"exp"`
	IssuedAt          int64    `json:"iat"`
	Nonce             string   `json:"nonce"`
	PreferredUsername string   `json:"preferred_username"`
	Name              string   `json:"name"`
	Email             string   `json:"email"`
	Role              string   `json:"role"`
	TicketRole        string   `json:"ticket_role"`
	AMR               []string `json:"amr"`
	AuthTime          int64    `json:"auth_time"`
}

// OpsSecondFactor reports whether this sign-in passed the authenticator
// app's code, the only second factor the ops entry takes. A text message
// ("sms") is for customer accounts and does not count here: operators open
// every customer's console, and a code sent to a phone number can be taken
// by swapping the SIM or forwarding the message.
func (c Claims) OpsSecondFactor() bool { return slices.Contains(c.AMR, "otp") }

// AuthenticatedWithin reports whether the person gave their password at most
// d before now, by the provider's word. A minute is allowed for clock drift.
// A token without auth_time does not pass: the provider ignored max_age.
func (c Claims) AuthenticatedWithin(d time.Duration, now time.Time) bool {
	if c.AuthTime == 0 {
		return false
	}
	at := time.Unix(c.AuthTime, 0)
	return !at.After(now.Add(time.Minute)) && now.Sub(at) <= d+time.Minute
}

// audience is a string or a list of strings, as the specification allows.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// ErrInvalidToken covers every way an ID token can fail its checks. The
// detail is for the log, never for the person signing in.
var ErrInvalidToken = errors.New("oidc: the ID token did not pass its checks")

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// discover reads the provider's metadata, once an hour at most.
func (p *Provider) discover(ctx context.Context) (metadata, error) {
	p.mu.Lock()
	if p.meta != nil && p.now().Sub(p.metaFetch) < time.Hour {
		m := *p.meta
		p.mu.Unlock()
		return m, nil
	}
	p.mu.Unlock()
	var m metadata
	if err := p.getJSON(ctx, strings.TrimRight(p.Issuer, "/")+"/.well-known/openid-configuration", &m); err != nil {
		return metadata{}, fmt.Errorf("oidc: discovery: %w", err)
	}
	// A provider that names another issuer is either misconfigured or not
	// the one this console was told to trust.
	if strings.TrimRight(m.Issuer, "/") != strings.TrimRight(p.Issuer, "/") {
		return metadata{}, fmt.Errorf("oidc: discovery names issuer %q, configured %q", m.Issuer, p.Issuer)
	}
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "" {
		return metadata{}, errors.New("oidc: discovery is missing an endpoint")
	}
	p.mu.Lock()
	p.meta, p.metaFetch = &m, p.now()
	p.mu.Unlock()
	return m, nil
}

func (p *Provider) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	res, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", res.StatusCode, u)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}

// Reachable asks the provider for its metadata now, not from the cache: a
// sign-in is about to send somebody there, and a provider that is down
// should be a message on this side rather than the browser's own error page.
func (p *Provider) Reachable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var m metadata
	return p.getJSON(ctx, strings.TrimRight(p.Issuer, "/")+"/.well-known/openid-configuration", &m)
}

// Flow is one sign-in in progress. Its fields stay on the server; only the
// state travels through the browser.
type Flow struct {
	State, Nonce, Verifier string
}

// NewFlow makes the secrets for one sign-in.
func NewFlow() (Flow, error) {
	var f Flow
	for _, s := range []*string{&f.State, &f.Nonce, &f.Verifier} {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return Flow{}, err
		}
		*s = base64.RawURLEncoding.EncodeToString(b)
	}
	return f, nil
}

// AuthURL is where to send the browser. A maxAge above zero asks the
// provider for the password and second factor again unless the person gave
// them within that long; Claims.AuthenticatedWithin checks it came true.
func (p *Provider) AuthURL(ctx context.Context, f Flow, redirectURI string, maxAge time.Duration) (string, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(f.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid profile email"},
		"state":                 {f.State},
		"nonce":                 {f.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if maxAge > 0 {
		q.Set("max_age", strconv.Itoa(int(maxAge.Seconds())))
	}
	sep := "?"
	if strings.Contains(m.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return m.AuthorizationEndpoint + sep + q.Encode(), nil
}

// LogoutURL ends the provider's own session, then comes back. Empty when
// the provider offers no way to.
func (p *Provider) LogoutURL(ctx context.Context, postLogout string) string {
	m, err := p.discover(ctx)
	if err != nil || m.EndSessionEndpoint == "" {
		return ""
	}
	q := url.Values{"client_id": {p.ClientID}, "post_logout_redirect_uri": {postLogout}}
	return m.EndSessionEndpoint + "?" + q.Encode()
}

// Exchange trades the code for an ID token and checks it against the flow
// that started the sign-in.
func (p *Provider) Exchange(ctx context.Context, f Flow, code, redirectURI string) (Claims, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return Claims{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {f.Verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Claims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(p.ClientSecret))
	res, err := p.client().Do(req)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: token request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Claims{}, fmt.Errorf("oidc: token request: HTTP %d", res.StatusCode)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&tok); err != nil || tok.IDToken == "" {
		return Claims{}, errors.New("oidc: the token reply has no ID token")
	}
	c, err := p.Verify(ctx, tok.IDToken)
	if err != nil {
		return Claims{}, err
	}
	if subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(f.Nonce)) != 1 {
		return Claims{}, fmt.Errorf("%w: nonce does not match the sign-in", ErrInvalidToken)
	}
	return c, nil
}

// clockSkew is how far the provider's clock may be from this one.
const clockSkew = 2 * time.Minute

// Verify checks an ID token's signature and standard claims.
func (p *Provider) Verify(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: not a JWT", ErrInvalidToken)
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &head); err != nil {
		return Claims{}, fmt.Errorf("%w: header: %v", ErrInvalidToken, err)
	}
	key, err := p.key(ctx, head.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: signature encoding", ErrInvalidToken)
	}
	if err := verifySignature(head.Alg, key, parts[0]+"."+parts[1], sig); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	var c Claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Claims{}, fmt.Errorf("%w: claims: %v", ErrInvalidToken, err)
	}
	now := p.now()
	switch {
	case strings.TrimRight(c.Issuer, "/") != strings.TrimRight(p.Issuer, "/"):
		return Claims{}, fmt.Errorf("%w: issuer %q", ErrInvalidToken, c.Issuer)
	case !slices.Contains([]string(c.Audience), p.ClientID):
		return Claims{}, fmt.Errorf("%w: audience %v", ErrInvalidToken, c.Audience)
	case len(c.Audience) > 1 && c.AuthorizedParty != p.ClientID:
		return Claims{}, fmt.Errorf("%w: several audiences and azp %q", ErrInvalidToken, c.AuthorizedParty)
	case c.Expiry == 0 || now.After(time.Unix(c.Expiry, 0).Add(clockSkew)):
		return Claims{}, fmt.Errorf("%w: expired", ErrInvalidToken)
	case c.IssuedAt != 0 && time.Unix(c.IssuedAt, 0).After(now.Add(clockSkew)):
		return Claims{}, fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	case c.Subject == "":
		return Claims{}, fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	c.Subject = strings.ToLower(c.Subject)
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	return c, nil
}

func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func verifySignature(alg string, key crypto.PublicKey, signed string, sig []byte) error {
	sum := sha256.Sum256([]byte(signed))
	switch alg {
	case "RS256":
		k, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("RS256 with a key that is not RSA")
		}
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig)
	case "ES256":
		k, ok := key.(*ecdsa.PublicKey)
		if !ok || k.Curve != elliptic.P256() || len(sig) != 64 {
			return errors.New("ES256 with a key or signature of the wrong shape")
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k, sum[:], r, s) {
			return errors.New("bad signature")
		}
		return nil
	}
	// "none" and the HMAC algorithms in particular: a token the provider did
	// not sign with its private key proves nothing.
	return fmt.Errorf("algorithm %q is not accepted", alg)
}

// key finds the signing key, reading the key set again when the id is new
// -- the provider rotated -- but not more than once a minute.
func (p *Provider) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	p.mu.Lock()
	k, ok := p.keys[kid]
	fresh := p.now().Sub(p.keysAt) < time.Minute
	p.mu.Unlock()
	if ok {
		return k, nil
	}
	if fresh {
		return nil, fmt.Errorf("%w: unknown key %q", ErrInvalidToken, kid)
	}
	m, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := p.getJSON(ctx, m.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("oidc: key set: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, j := range set.Keys {
		if pk, err := j.public(); err == nil {
			keys[j.Kid] = pk
		}
	}
	p.mu.Lock()
	p.keys, p.keysAt = keys, p.now()
	p.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown key %q", ErrInvalidToken, kid)
}

type jwk struct {
	Kty, Kid, Use, Crv string
	N, E, X, Y         string
}

func (j jwk) public() (crypto.PublicKey, error) {
	if j.Use != "" && j.Use != "sig" {
		return nil, errors.New("not a signing key")
	}
	dec := func(s string) (*big.Int, error) {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil || len(b) == 0 {
			return nil, errors.New("bad key number")
		}
		return new(big.Int).SetBytes(b), nil
	}
	switch j.Kty {
	case "RSA":
		n, err := dec(j.N)
		if err != nil {
			return nil, err
		}
		e, err := dec(j.E)
		if err != nil || !e.IsInt64() {
			return nil, errors.New("bad exponent")
		}
		if n.BitLen() < 2048 {
			return nil, errors.New("RSA key shorter than 2048 bits")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		if j.Crv != "P-256" {
			return nil, errors.New("only P-256")
		}
		x, err := dec(j.X)
		if err != nil {
			return nil, err
		}
		y, err := dec(j.Y)
		if err != nil {
			return nil, err
		}
		k := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		if !k.Curve.IsOnCurve(x, y) {
			return nil, errors.New("point not on curve")
		}
		return k, nil
	}
	return nil, fmt.Errorf("key type %q", j.Kty)
}
