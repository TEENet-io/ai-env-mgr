// Package agentapi is the agent's side of the console's device API.
package agentapi

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

	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// Errors a caller decides on.
var (
	// ErrUnauthorized means the device token is not accepted any more:
	// revoked, or the machine was forgotten. The agent enrols again.
	ErrUnauthorized = errors.New("the console does not accept this machine's token")
	// ErrAlreadyEnrolled means the console holds a live token for this
	// hostname and an administrator has not allowed re-enrolment.
	ErrAlreadyEnrolled = errors.New("the console already holds a token for this machine")
	// ErrNotFound is a 404: no credentials published, no such artifact.
	ErrNotFound = errors.New("not found")
)

// Client talks to one console with one device token.
type Client struct {
	BaseURL string
	Token   string
	Version string // sent as X-Agent-Version
	HTTP    *http.Client
}

const userAgent = "ai-env-agent"

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) url(path string) string {
	return strings.TrimSuffix(c.BaseURL, "/") + path
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, contentType string) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rd)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Version != "" {
		req.Header.Set("X-Agent-Version", c.Version)
	}
	req.Header.Set("User-Agent", userAgent+"/"+c.Version)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// do sends the request and turns the status codes every call shares into
// errors; the caller reads the rest.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	res, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		res.Body.Close()
		return nil, ErrUnauthorized
	case http.StatusNotFound:
		res.Body.Close()
		return nil, ErrNotFound
	}
	if res.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		res.Body.Close()
		return nil, fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Path, res.Status, strings.TrimSpace(string(msg)))
	}
	return res, nil
}

type enrolRequest struct {
	Hostname     string `json:"hostname"`
	AgentVersion string `json:"agentVersion"`
}

type enrolResponse struct {
	DeviceID    string `json:"deviceId"`
	DeviceToken string `json:"deviceToken"`
}

// Enrol asks the console for this machine's token. Nothing but the
// hostname identifies the machine; the token that comes back is what
// identifies it from then on.
func Enrol(ctx context.Context, baseURL, hostname, version string, httpClient *http.Client) (deviceID, token string, err error) {
	c := &Client{BaseURL: baseURL, Version: version, HTTP: httpClient}
	body, _ := json.Marshal(enrolRequest{Hostname: hostname, AgentVersion: version})
	req, err := c.request(ctx, http.MethodPost, "/agent/v1/enrol", body, "application/json")
	if err != nil {
		return "", "", err
	}
	res, err := c.http().Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		var out enrolResponse
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			return "", "", fmt.Errorf("enrol: unreadable reply: %w", err)
		}
		if out.DeviceToken == "" {
			return "", "", errors.New("enrol: the reply carried no token")
		}
		return out.DeviceID, out.DeviceToken, nil
	case http.StatusConflict:
		return "", "", ErrAlreadyEnrolled
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", "", fmt.Errorf("enrol: %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
}

// Config fetches the machine's configuration. With ifNoneMatch set to the
// etag last seen, an unchanged configuration comes back as unchanged.
func (c *Client) Config(ctx context.Context, ifNoneMatch string) (*deviceconfig.Config, bool, error) {
	req, err := c.request(ctx, http.MethodGet, "/agent/v1/config", nil, "")
	if err != nil {
		return nil, false, err
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	res, err := c.do(req)
	if err != nil {
		return nil, false, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	var cfg deviceconfig.Config
	if err := json.NewDecoder(res.Body).Decode(&cfg); err != nil {
		return nil, false, fmt.Errorf("config: unreadable reply: %w", err)
	}
	return &cfg, false, nil
}

// Wait holds until the configuration differs from etag or the console's
// patience runs out. changed false means "nothing yet, ask again".
func (c *Client) Wait(ctx context.Context, etag string) (*deviceconfig.Config, bool, error) {
	req, err := c.request(ctx, http.MethodGet, "/agent/v1/wait?etag="+etag, nil, "")
	if err != nil {
		return nil, false, err
	}
	// The console holds for 25 seconds; give it that and some.
	client := *c.http()
	client.Timeout = 40 * time.Second
	res, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent:
		return nil, false, nil
	case http.StatusUnauthorized:
		return nil, false, ErrUnauthorized
	case http.StatusOK:
		var cfg deviceconfig.Config
		if err := json.NewDecoder(res.Body).Decode(&cfg); err != nil {
			return nil, false, fmt.Errorf("wait: unreadable reply: %w", err)
		}
		return &cfg, true, nil
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, false, fmt.Errorf("wait: %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
}

// Credentials fetches the assignee's bundle. exists false: nothing
// published (or not assigned). unchanged true: the etag matched.
func (c *Client) Credentials(ctx context.Context, ifNoneMatch string) (data []byte, etag string, exists, unchanged bool, err error) {
	req, err := c.request(ctx, http.MethodGet, "/agent/v1/credentials", nil, "")
	if err != nil {
		return nil, "", false, false, err
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	res, err := c.do(req)
	if errors.Is(err, ErrNotFound) {
		return nil, "", false, false, nil
	}
	if err != nil {
		return nil, "", false, false, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified {
		return nil, ifNoneMatch, true, true, nil
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, "", false, false, err
	}
	return data, res.Header.Get("ETag"), true, false, nil
}

func (c *Client) PostStatus(ctx context.Context, data []byte) error {
	req, err := c.request(ctx, http.MethodPost, "/agent/v1/status", data, "application/json")
	if err != nil {
		return err
	}
	res, err := c.do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

func (c *Client) PostLog(ctx context.Context, tail []byte) error {
	req, err := c.request(ctx, http.MethodPost, "/agent/v1/log", tail, "text/plain; charset=utf-8")
	if err != nil {
		return err
	}
	res, err := c.do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// ArtifactURL asks where to download a targeted build from, and what its
// SHA-256 is. The link is temporary and carries no credentials.
func (c *Client) ArtifactURL(ctx context.Context, product, version string) (link, sha256hex string, err error) {
	req, err := c.request(ctx, http.MethodGet, "/agent/v1/artifact/"+product+"/"+version, nil, "")
	if err != nil {
		return "", "", err
	}
	client := *c.http()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusFound, http.StatusTemporaryRedirect:
		return res.Header.Get("Location"), res.Header.Get("X-Artifact-SHA256"), nil
	case http.StatusUnauthorized:
		return "", "", ErrUnauthorized
	case http.StatusNotFound:
		return "", "", ErrNotFound
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", "", fmt.Errorf("artifact %s %s: %s: %s", product, version, res.Status, strings.TrimSpace(string(msg)))
	}
}

func (c *Client) ApplicationManifest(ctx context.Context, appID, version string) (model.Application, error) {
	var out model.Application
	req, err := c.request(ctx, http.MethodGet, "/agent/v1/application/"+url.PathEscape(appID)+"/"+url.PathEscape(version)+"/manifest", nil, "")
	if err != nil {
		return out, err
	}
	res, err := c.do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("application manifest: unreadable reply: %w", err)
	}
	return out, nil
}

func (c *Client) ApplicationURL(ctx context.Context, appID, version string) (link, sha256hex string, err error) {
	req, err := c.request(ctx, http.MethodGet, "/agent/v1/application/"+url.PathEscape(appID)+"/"+url.PathEscape(version), nil, "")
	if err != nil {
		return "", "", err
	}
	client := *c.http()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusFound, http.StatusTemporaryRedirect:
		return res.Header.Get("Location"), res.Header.Get("X-Artifact-SHA256"), nil
	case http.StatusUnauthorized:
		return "", "", ErrUnauthorized
	case http.StatusNotFound:
		return "", "", ErrNotFound
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", "", fmt.Errorf("application %s@%s: %s: %s", appID, version, res.Status, strings.TrimSpace(string(msg)))
	}
}

// CollectUploadURL asks for a signed link to upload one session file of
// the machine's assignee.
func (c *Client) CollectUploadURL(ctx context.Context, user, rel string) (link, contentType string, err error) {
	body, _ := json.Marshal(map[string]string{"user": user, "rel": rel})
	req, err := c.request(ctx, http.MethodPost, "/agent/v1/collect/upload-url", body, "application/json")
	if err != nil {
		return "", "", err
	}
	res, err := c.do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	var out struct {
		URL         string `json:"url"`
		ContentType string `json:"contentType"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("upload url: unreadable reply: %w", err)
	}
	return out.URL, out.ContentType, nil
}

// Rotate asks for a new token. The old one keeps working briefly.
func (c *Client) Rotate(ctx context.Context) (string, error) {
	req, err := c.request(ctx, http.MethodPost, "/agent/v1/token/rotate", nil, "")
	if err != nil {
		return "", err
	}
	res, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		Token string `json:"deviceToken"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || out.Token == "" {
		return "", errors.New("rotate: the reply carried no token")
	}
	return out.Token, nil
}
