// Package ghrelease downloads a release asset from GitHub, including from a
// private repository.
//
// It exists as one implementation because the CLI and the web console both
// need it, and two copies of a fetch drift: they had already grown slightly
// different error messages for the same failure.
package ghrelease

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// browserURL matches the address a person copies out of the releases page:
//
//	https://github.com/OWNER/REPO/releases/download/TAG/FILE
//
// It is the obvious thing to paste and the one thing that cannot work for a
// private repository -- github.com answers 404 there regardless of the token,
// because release assets are served from a different host that authenticates
// differently. The API has to be asked for the asset's own URL instead.
var browserURL = regexp.MustCompile(
	`^https://github\.com/([^/]+)/([^/]+)/releases/download/([^/]+)/(.+)$`)

// apiBase is overridable in tests.
var apiBase = "https://api.github.com"

// Fetch downloads what the URL points at.
//
// A browser-style release URL is resolved through the API first, so the
// address anybody would naturally paste works for a private repository too.
// Any other URL is fetched directly.
//
// timeout covers the whole transfer: the Codex installer is ~700 MB, so the
// caller passes something generous.
func Fetch(url, token string, timeout time.Duration) ([]byte, error) {
	return FetchProgress(url, token, timeout, nil)
}

// FetchProgress is Fetch, reporting bytes as they arrive.
//
// onProgress is called with what has been read and the total the server
// declared, which is -1 when it declared none. It exists because the caller is
// a web page watching a ~700 MB transfer: elapsed seconds alone cannot tell a
// slow download from a stalled one.
func FetchProgress(url, token string, timeout time.Duration, onProgress func(done, total int64)) ([]byte, error) {
	client := &http.Client{Timeout: timeout}

	if m := browserURL.FindStringSubmatch(url); m != nil {
		owner, repo, tag, name := m[1], m[2], m[3], m[4]
		assetURL, err := assetAPIURL(client, owner, repo, tag, name, token)
		if err != nil {
			return nil, err
		}
		url = assetURL
	}
	return get(client, url, token, onProgress)
}

// assetAPIURL asks the API for the download URL of one named asset.
func assetAPIURL(client *http.Client, owner, repo, tag, name, token string) (string, error) {
	api := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", apiBase, owner, repo, tag)
	req, err := http.NewRequest(http.MethodGet, api, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("look up release %s: %w", tag, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// GitHub hides private repositories rather than admitting they exist,
		// so 404 here is usually a permission problem, not a missing release.
		if token == "" {
			return "", fmt.Errorf("release %s not found; a private repository also needs a token", tag)
		}
		return "", fmt.Errorf("release %s not found, or the token cannot read %s/%s "+
			"(a fine-grained token needs Contents: Read on that repository)", tag, owner, repo)
	case http.StatusUnauthorized:
		return "", fmt.Errorf("the token was rejected; check it has not expired")
	default:
		return "", fmt.Errorf("looking up release %s returned HTTP %d", tag, resp.StatusCode)
	}

	var release struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"` // the API URL, which accepts a token
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("read release %s: %w", tag, err)
	}
	var names []string
	for _, a := range release.Assets {
		if a.Name == name {
			return a.URL, nil
		}
		names = append(names, a.Name)
	}
	// Listing what is there turns "404" into something actionable.
	return "", fmt.Errorf("release %s has no asset named %q; it has: %s",
		tag, name, strings.Join(names, ", "))
}

func get(client *http.Client, url, token string, onProgress func(done, total int64)) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return nil, fmt.Errorf("the URL must start with http:// or https://")
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// octet-stream is what makes the API return the asset itself rather than
	// its JSON description.
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound && token == "" {
			return nil, fmt.Errorf("the download returned 404; a private release also needs a token")
		}
		return nil, fmt.Errorf("the download returned HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		return nil, fmt.Errorf("the download returned HTML rather than a file, "+
			"which usually means an auth wall (Content-Type %q)", ct)
	}
	if onProgress == nil {
		return io.ReadAll(resp.Body)
	}
	// Size the buffer from Content-Length when the server gave one, so a
	// 700 MB asset is not grown by repeated reallocation.
	var buf bytes.Buffer
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	}
	onProgress(0, resp.ContentLength)
	if _, err := io.Copy(&buf, &countingReader{
		r: resp.Body, total: resp.ContentLength, report: onProgress,
	}); err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	return buf.Bytes(), nil
}

// countingReader reports progress as it is read through.
//
// Reports are throttled: a 700 MB transfer is tens of thousands of reads and
// the receiver takes a lock, which is worth neither the contention nor the
// wake-ups for updates a page redraws every few seconds anyway.
type countingReader struct {
	r        io.Reader
	total    int64
	done     int64
	reported int64
	report   func(done, total int64)
}

// progressStep is how much has to move before another report is worth making.
const progressStep = 4 << 20 // 4 MiB

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
	if c.done-c.reported >= progressStep || (err == io.EOF && c.done != c.reported) {
		c.reported = c.done
		c.report(c.done, c.total)
	}
	return n, err
}
