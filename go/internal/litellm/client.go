// Package litellm talks to the LiteLLM gateway's management API.
//
// The gateway owns two things this tool needs: the per-employee tokens that
// let Codex reach a model at all, and the catalog metadata that decides what
// the model picker shows. Both are read and written here; nothing about the
// model line-up is duplicated on the admin side, so adding a model to the
// gateway is enough to make it appear for employees.
package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls the gateway's management endpoints.
//
// The admin key is held in memory only, like every other credential this
// console handles: it is never written to disk, logged, or placed in a
// cookie. It is a proxy_admin key rather than the gateway's master key, so
// it can be revoked on its own if it leaks.
type Client struct {
	BaseURL  string
	AdminKey string
	HTTP     *http.Client
}

// New returns a client for a gateway at baseURL (e.g.
// "https://litellm.teenet.app").
//
// The timeout is generous because these calls cross the public internet to
// another cloud, but it is still bounded: an onboarding request that hangs
// forever would leave the administrator staring at a spinner with no way to
// tell whether the token was created.
func New(baseURL, adminKey string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		AdminKey: adminKey,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Key is a per-employee token as the gateway reports it.
//
// Key (the plaintext) is present only in the response to /key/generate.
// Everywhere else -- /key/list in particular -- the gateway reports the
// hashed Token instead, so code that revokes or updates a token it did not
// just mint must not reach for Key: it will be empty, and the gateway
// answers an empty key with 404 "No keys found".
type Key struct {
	Key       string   `json:"key,omitempty"`   // plaintext; /key/generate only
	Token     string   `json:"token,omitempty"` // sha-256 of the plaintext; what /key/list returns
	KeyAlias  string   `json:"key_alias"`
	Models    []string `json:"models"`
	MaxBudget *float64 `json:"max_budget,omitempty"`
	Spend     float64  `json:"spend,omitempty"`
}

// ModelInfo carries the catalog metadata a model declares in the gateway's
// config.yaml. These fields are ours, not LiteLLM's: the gateway passes
// unknown model_info keys through verbatim, which is what lets the gateway
// be the single source of truth for the picker.
type ModelInfo struct {
	DisplayName      string   `json:"display_name"`
	ContextWindow    int      `json:"context_window"`
	ReasoningLevels  []string `json:"reasoning_levels"`
	DefaultReasoning string   `json:"default_reasoning"`
	Modalities       []string `json:"modalities"`
	CatalogVisible   bool     `json:"catalog_visible"`
}

// Model pairs a routing key with its catalog metadata.
//
// Name is both the gateway's routing key and the catalog slug the picker
// sends back. The two cannot drift: a slug with no matching model_name
// routes nowhere.
type Model struct {
	Name string    `json:"model_name"`
	Info ModelInfo `json:"model_info"`
}

// GenerateKey mints a token for one employee.
//
// alias must be stable per employee: the gateway enforces global uniqueness
// on it, which is what stops a double-click from issuing two live tokens for
// the same person. The plaintext token is returned once and never again, so
// the caller must deliver it before discarding the response.
func (c *Client) GenerateKey(ctx context.Context, alias string, models []string, maxBudget float64, metadata map[string]string) (Key, error) {
	body := map[string]any{
		"key_alias": alias,
		"models":    models,
	}
	if maxBudget > 0 {
		body["max_budget"] = maxBudget
	}
	if len(metadata) > 0 {
		body["metadata"] = metadata
	}

	var out Key
	if err := c.do(ctx, http.MethodPost, "/key/generate", body, &out); err != nil {
		return Key{}, err
	}
	if out.Key == "" {
		return Key{}, fmt.Errorf("gateway accepted /key/generate for %q but returned no token", alias)
	}
	return out, nil
}

// Handle returns the identifier the gateway accepts for updating or
// revoking this token: the plaintext when it is known, otherwise the hash.
// The gateway's /key/update and /key/delete take either (they hash a
// plaintext themselves), so callers need not care which they hold.
func (k Key) Handle() string {
	if k.Key != "" {
		return k.Key
	}
	return k.Token
}

// UpdateKey changes which models a token may use. handle is Key.Handle().
//
// This takes effect at the gateway immediately and does not touch the
// employee's machine. It governs what they *can* use; the catalog governs
// what they can *see*, so a permission change normally needs both.
func (c *Client) UpdateKey(ctx context.Context, handle string, models []string) error {
	if handle == "" {
		return fmt.Errorf("update key: no token handle (neither plaintext nor hash)")
	}
	return c.do(ctx, http.MethodPost, "/key/update", map[string]any{
		"key":    handle,
		"models": models,
	}, nil)
}

// DeleteKey revokes tokens by handle (plaintext or hash). Revocation is
// server-side and takes effect at once, so it does not depend on the
// employee's machine being reachable -- which is the whole reason
// offboarding does not rely on the agent alone.
//
// An empty handle is refused here rather than sent: the gateway answers it
// with 404 "No keys found", which reads as "already gone" and would let a
// live token survive a revocation that reported success.
func (c *Client) DeleteKey(ctx context.Context, handles ...string) error {
	if len(handles) == 0 {
		return nil
	}
	for _, h := range handles {
		if h == "" {
			return fmt.Errorf("delete key: empty token handle")
		}
	}
	return c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"keys": handles}, nil)
}

// DeleteKeyByAlias revokes whatever token carries alias.
//
// This is the form offboarding uses. The alias is the one handle this console
// derives itself (see admincore.KeyAlias) and therefore always has, whereas
// the plaintext is gone the moment /key/generate returns and the hash has to
// be fetched first.
func (c *Client) DeleteKeyByAlias(ctx context.Context, alias string) error {
	if alias == "" {
		return fmt.Errorf("delete key: empty alias")
	}
	return c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"key_aliases": []string{alias}}, nil)
}

// FindKeyByAlias locates an existing token by its alias.
//
// The gateway has no lookup-by-alias endpoint (/key/info answers 404 for an
// alias), so this lists and filters. It exists for reconciliation: after a
// failed onboarding the token may exist with no configuration delivered, and
// the alias is the only handle left to find it by.
func (c *Client) FindKeyByAlias(ctx context.Context, alias string) (Key, bool, error) {
	keys, err := c.ListKeys(ctx)
	if err != nil {
		return Key{}, false, err
	}
	for _, k := range keys {
		if k.KeyAlias == alias {
			return k, true, nil
		}
	}
	return Key{}, false, nil
}

// keyPageSize is the gateway's hard ceiling for /key/list. Asking for more
// is a 422, not a silently smaller page.
const keyPageSize = 100

// keyPageLimit bounds the paging loop. It is a runaway guard, not a real
// limit: at 100 per page it covers 20,000 tokens, far past any roster this
// console manages.
const keyPageLimit = 200

// ListKeys returns every token the gateway knows about, following pagination
// to the end.
//
// Reading only the first page would be worse than an error here. This list is
// what reconciliation compares the roster against, and a token missing from
// it reads as "already revoked" -- so a truncated read would quietly report
// a departed employee's live token as gone.
func (c *Client) ListKeys(ctx context.Context) ([]Key, error) {
	var all []Key
	for page := 1; page <= keyPageLimit; page++ {
		var out struct {
			Keys       []Key `json:"keys"`
			TotalPages int   `json:"total_pages"`
		}
		path := fmt.Sprintf("/key/list?return_full_object=true&size=%d&page=%d", keyPageSize, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Keys...)

		// Stop on the reported last page, and also on a short or empty page
		// in case total_pages is ever absent -- either way the alternative
		// is looping until the guard trips.
		if page >= out.TotalPages || len(out.Keys) < keyPageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("gateway /key/list did not terminate after %d pages", keyPageLimit)
}

// Models returns the gateway's catalog metadata.
//
// Only models marked catalog_visible are returned: a model may be routable
// for testing without being something employees should see in the picker.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Data []Model `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/model/info", nil, &out); err != nil {
		return nil, err
	}
	visible := make([]Model, 0, len(out.Data))
	for _, m := range out.Data {
		if m.Info.CatalogVisible {
			visible = append(visible, m)
		}
	}
	return visible, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s request: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.AdminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("call gateway %s: %w", path, err)
	}
	defer resp.Body.Close()

	// Cap the read: a misrouted request can land on something that streams
	// indefinitely, and the management API's responses are small.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read gateway %s response: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Path: path, Body: strings.TrimSpace(string(payload))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode gateway %s response: %w", path, err)
	}
	return nil
}

// APIError is a non-2xx answer from the gateway. The status is kept separate
// from the message so callers can act on it -- notably 400 on /key/generate,
// which means the alias is already taken rather than that anything is broken.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gateway %s returned %d: %s", e.Path, e.Status, e.Body)
}

// IsAliasTaken reports whether err is the gateway refusing a duplicate alias.
//
// This is the signal that an employee already has a live token: re-onboarding
// must revoke the old one first rather than inventing a new alias, because a
// random alias would break the ability to reconcile tokens against the roster.
func IsAliasTaken(err error) bool {
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusBadRequest && strings.Contains(apiErr.Body, "already exists")
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
