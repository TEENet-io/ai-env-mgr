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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	Key      string   `json:"key,omitempty"`   // plaintext; /key/generate only
	Token    string   `json:"token,omitempty"` // sha-256 of the plaintext; what /key/list returns
	KeyAlias string   `json:"key_alias"`
	UserID   string   `json:"user_id,omitempty"` // the internal user whose limits this key inherits
	Models   []string `json:"models"`
	Spend    float64  `json:"spend,omitempty"`
}

// ModelInfo carries the catalog metadata a model declares in the gateway's
// config.yaml. These fields are ours, not LiteLLM's: the gateway passes
// unknown model_info keys through verbatim, which is what lets the gateway
// be the single source of truth for the picker.
type ModelInfo struct {
	ID               string   `json:"id"`
	Mode             string   `json:"mode"`
	DisplayName      string   `json:"display_name"`
	ContextWindow    int      `json:"context_window"`
	ReasoningLevels  []string `json:"reasoning_levels"`
	DefaultReasoning string   `json:"default_reasoning"`
	Modalities       []string `json:"modalities"`
	CatalogVisible   bool     `json:"catalog_visible"`

	// LitellmProvider is LiteLLM's own name for how it calls the model
	// ("bedrock", "vertex_ai", "openai", ...). It is not the supplier:
	// Azure deployments reached through the OpenAI-compatible route say
	// "openai". See ChannelOf.
	LitellmProvider string `json:"litellm_provider"`
	// Channel, when an administrator sets it in the gateway's model_info,
	// names the supplier outright and wins over what ChannelOf infers.
	Channel string `json:"channel"`
}

// ModelParams is the part of a deployment's litellm_params ChannelOf reads.
type ModelParams struct {
	Model   string `json:"model"`
	APIBase string `json:"api_base"`
}

// Model pairs a routing key with its catalog metadata.
//
// Name is both the gateway's routing key and the catalog slug the picker
// sends back. The two cannot drift: a slug with no matching model_name
// routes nowhere.
type Model struct {
	Name   string      `json:"model_name"`
	Info   ModelInfo   `json:"model_info"`
	Params ModelParams `json:"litellm_params"`
}

// GenerateKey mints a token for one employee, owned by userID.
//
// alias must be stable per employee: the gateway enforces global uniqueness
// on it, which is what stops a double-click from issuing two live tokens for
// the same person. The plaintext token is returned once and never again, so
// the caller must deliver it before discarding the response.
//
// The key carries no budget or rate limit of its own. Those live on the
// user (see UpsertUser) so that they survive re-issuing the token; a limit
// on the key would reset to zero spend every time the token was replaced.
func (c *Client) GenerateKey(ctx context.Context, alias, userID string, models []string, metadata map[string]string) (Key, error) {
	if userID == "" {
		return Key{}, fmt.Errorf("generate key %q: no owning user", alias)
	}
	body := map[string]any{
		"key_alias": alias,
		"user_id":   userID,
		"models":    models,
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

// AllModels returns every gateway deployment, including hidden backups.
// Channel diagnostics must see those too; employee pickers must not.
func (c *Client) AllModels(ctx context.Context) ([]Model, error) {
	var out struct {
		Data []Model `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/model/info", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Models returns only deployments visible in the employee catalog.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	all, err := c.AllModels(ctx)
	if err != nil {
		return nil, err
	}
	visible := make([]Model, 0, len(all))
	for _, m := range all {
		if m.Info.CatalogVisible {
			visible = append(visible, m)
		}
	}
	return visible, nil
}

// DeploymentHealth is the result of a health check scoped to one deployment.
// A 503 with an unhealthy_endpoints body is a completed failed check, not a
// transport error. No provider credentials or endpoint URLs are retained.
type DeploymentHealth struct {
	HealthyCount   int `json:"healthy_count"`
	UnhealthyCount int `json:"unhealthy_count"`
	Healthy        []struct {
		ModelID string `json:"model_id"`
	} `json:"healthy_endpoints"`
	Unhealthy []struct {
		ModelID string `json:"model_id"`
	} `json:"unhealthy_endpoints"`
}

func (c *Client) CheckDeployment(ctx context.Context, modelID string) (DeploymentHealth, error) {
	if modelID == "" {
		return DeploymentHealth{}, fmt.Errorf("health check: no deployment ID")
	}
	var out DeploymentHealth
	path := "/health?model_id=" + url.QueryEscape(modelID)
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	if err != nil {
		var apiErr *APIError
		if !asAPIError(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || json.Unmarshal([]byte(apiErr.Body), &out) != nil {
			return DeploymentHealth{}, err
		}
	}
	if out.HealthyCount+out.UnhealthyCount != 1 {
		return DeploymentHealth{}, fmt.Errorf("health check returned %d deployments, expected one", out.HealthyCount+out.UnhealthyCount)
	}
	if out.HealthyCount == 1 {
		if len(out.Healthy) != 1 || out.Healthy[0].ModelID != modelID {
			return DeploymentHealth{}, fmt.Errorf("health check returned another deployment")
		}
	} else if len(out.Unhealthy) != 1 || out.Unhealthy[0].ModelID != modelID {
		return DeploymentHealth{}, fmt.Errorf("health check returned another deployment")
	}
	return out, nil
}

// ChatTrial sends a small, non-streaming prompt to one exact deployment. The
// caller must still validate that the deployment belongs to the requested
// channel. Prompt and output are returned to the caller only; they are not
// recorded in the console's audit trail.
type ChatTrial struct {
	Output           string `json:"output"`
	ModelID          string `json:"model_id"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	FinishReason     string `json:"finish_reason"`
}

type ChatTrialOptions struct {
	DisableThinking bool
}

var ErrNoVisibleOutput = errors.New("gateway chat response had no visible text")

func (c *Client) TryChat(ctx context.Context, modelID, prompt string, opts ChatTrialOptions) (ChatTrial, error) {
	if modelID == "" || strings.TrimSpace(prompt) == "" {
		return ChatTrial{}, fmt.Errorf("chat trial requires a deployment and prompt")
	}
	params := map[string]any{
		"model":      modelID,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 256,
		"stream":     false,
		"metadata":   map[string]string{"request_source": "windows-control-channel-test"},
	}
	if opts.DisableThinking {
		params["thinking"] = map[string]any{"type": "disabled", "budget_tokens": 0}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return ChatTrial{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatTrial{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AdminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ChatTrial{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ChatTrial{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Provider errors can contain credentials or echoed prompts. Only the
		// status is propagated to the admin page.
		return ChatTrial{}, &APIError{Status: resp.StatusCode, Path: "/chat/completions"}
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return ChatTrial{}, fmt.Errorf("decode gateway chat response: %w", err)
	}
	if len(out.Choices) == 0 {
		return ChatTrial{}, fmt.Errorf("gateway chat response had no choices")
	}
	routedID := resp.Header.Get("x-litellm-model-id")
	if routedID == "" {
		return ChatTrial{}, fmt.Errorf("gateway did not identify the serving deployment")
	}
	if routedID != modelID {
		return ChatTrial{}, fmt.Errorf("gateway routed the trial to another deployment")
	}
	trial := ChatTrial{
		Output:           out.Choices[0].Message.Content,
		ModelID:          routedID,
		PromptTokens:     out.Usage.PromptTokens,
		CompletionTokens: out.Usage.CompletionTokens,
		FinishReason:     out.Choices[0].FinishReason,
	}
	if strings.TrimSpace(trial.Output) == "" {
		return trial, ErrNoVisibleOutput
	}
	return trial, nil
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
