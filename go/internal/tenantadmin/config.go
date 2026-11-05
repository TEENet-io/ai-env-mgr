// Package tenantadmin lets each company's console manage its own LiteLLM team
// on a shared gateway, and nothing else.
//
// Open-source LiteLLM has only gateway-wide administrator keys, so a console
// holding one could read and change every other company's users and keys.
// tenant-admin holds that key instead. A console presents a tenant token,
// which this service maps to one team, and each request is checked against
// the rules in rules.go before it is replayed to LiteLLM under the
// administrator key. Design: docs/superpowers/specs/2026-11-05-tenant-admin-proxy-design.md.
package tenantadmin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
)

// Config is tenants.json: who may call, and as which team. It holds token
// hashes only; the plaintext is shown once, when the token is made.
type Config struct {
	Tenants []Tenant `json:"tenants"`
}

type Tenant struct {
	Team    string  `json:"team"`
	Enabled bool    `json:"enabled"`
	Tokens  []Token `json:"tokens"`
}

// Token is one credential of a tenant. A tenant may hold two at once, so a
// console can be moved to a new token before the old one is removed.
type Token struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// LoadConfig reads and checks tenants.json. A missing file is an empty
// configuration: the service then refuses everyone, which is the safe way
// to start before the first tenant is added.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	teams := map[string]bool{}
	ids := map[string]bool{}
	for _, t := range c.Tenants {
		if t.Team == "" {
			return errors.New("a tenant has no team")
		}
		if err := litellm.ValidateTeamID(t.Team); err != nil {
			return err
		}
		if teams[t.Team] {
			return fmt.Errorf("team %q is listed twice", t.Team)
		}
		teams[t.Team] = true
		for _, tok := range t.Tokens {
			if ids[tok.ID] {
				return fmt.Errorf("token id %q is used twice", tok.ID)
			}
			ids[tok.ID] = true
			if b, err := hex.DecodeString(tok.SHA256); err != nil || len(b) != sha256.Size {
				return fmt.Errorf("token %q: sha256 is not a SHA-256 hex digest", tok.ID)
			}
		}
	}
	return nil
}

// Save writes the configuration readable by its owner only, replacing the
// file in one rename so the service never reads half of it.
func (c Config) Save(path string) error {
	if err := c.validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tenants-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Identity is who a valid token belongs to.
type Identity struct {
	Team    string
	TokenID string
}

// Authenticate maps a presented token to its tenant. Every enabled token is
// compared, in constant time, so the answer's timing does not depend on
// which tenant or how much of a digest matched.
func (c Config) Authenticate(token string) (Identity, bool) {
	if token == "" {
		return Identity{}, false
	}
	sum := sha256.Sum256([]byte(token))
	var found Identity
	ok := false
	for _, t := range c.Tenants {
		for _, tok := range t.Tokens {
			want, err := hex.DecodeString(tok.SHA256)
			if err != nil {
				continue
			}
			if subtle.ConstantTimeCompare(sum[:], want) == 1 && t.Enabled {
				found, ok = Identity{Team: t.Team, TokenID: tok.ID}, true
			}
		}
	}
	return found, ok
}

// tokenPrefix marks tenant tokens, so one pasted in the wrong place is
// recognisable.
const tokenPrefix = "ta-"

// NewToken returns a fresh tenant token and its stored digest.
func NewToken() (plain, digest string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plain = tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(plain))
	return plain, hex.EncodeToString(sum[:]), nil
}

// AddToken records a new token for team, adding the tenant if it is new,
// and returns the token's id.
func (c *Config) AddToken(team, digest string) (string, error) {
	if err := litellm.ValidateTeamID(team); err != nil || team == "" {
		return "", fmt.Errorf("team %q: use 2-32 lower-case letters, digits or underscores", team)
	}
	i := -1
	for j := range c.Tenants {
		if c.Tenants[j].Team == team {
			i = j
		}
	}
	if i < 0 {
		c.Tenants = append(c.Tenants, Tenant{Team: team, Enabled: true})
		i = len(c.Tenants) - 1
	}
	used := map[string]bool{}
	for _, t := range c.Tenants {
		for _, tok := range t.Tokens {
			used[tok.ID] = true
		}
	}
	n := 1
	for used[fmt.Sprintf("%s-%d", team, n)] {
		n++
	}
	id := fmt.Sprintf("%s-%d", team, n)
	c.Tenants[i].Tokens = append(c.Tenants[i].Tokens, Token{ID: id, SHA256: digest})
	return id, nil
}

// RemoveToken drops the token with id. Removing the last token leaves the
// tenant listed with none, which refuses every request for that team.
func (c *Config) RemoveToken(id string) error {
	for i := range c.Tenants {
		for j, tok := range c.Tenants[i].Tokens {
			if tok.ID == id {
				c.Tenants[i].Tokens = append(c.Tenants[i].Tokens[:j], c.Tenants[i].Tokens[j+1:]...)
				return nil
			}
		}
	}
	return fmt.Errorf("no token %q", id)
}

// bearer extracts the token from an Authorization header.
func bearer(header string) string {
	const p = "Bearer "
	if len(header) > len(p) && strings.EqualFold(header[:len(p)], p) {
		return strings.TrimSpace(header[len(p):])
	}
	return ""
}
