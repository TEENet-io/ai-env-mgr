// Package ghrelease downloads a release asset from GitHub, including from a
// private repository.
//
// It exists as one implementation because the CLI and the web console both
// need it, and two copies of a fetch drift: they had already grown slightly
// different error messages for the same failure.
package ghrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
// It goes through a temporary file: the streaming path is the one
// implementation, and the callers that want bytes (the CLI, packages of a
// few megabytes) read them back. onProgress is called with what has been
// read and the total the server declared, -1 when it declared none.
func FetchProgress(url, token string, timeout time.Duration, onProgress func(done, total int64)) ([]byte, error) {
	tmp, err := os.CreateTemp("", "ghrelease-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	if _, err := FetchToFile(url, token, timeout, name, 0, onProgress); err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}

// Fetched describes a download that reached disk whole.
type Fetched struct {
	SHA256 string // hex
	Size   int64
}

// FetchToFile downloads url to dest, hashing as it goes, and never holds
// more than a buffer of the body in memory. It refuses a body larger than
// maxBytes (0 = no limit) and removes dest on any failure, so a caller
// either has a complete, checksummed file or nothing.
//
// A partial file is worse than none: the next step uploads what is on disk,
// and a truncated installer with a checksum computed over the truncation
// would pass every check on the way to a desktop.
func FetchToFile(url, token string, timeout time.Duration, dest string, maxBytes int64, onProgress func(done, total int64)) (Fetched, error) {
	client := &http.Client{Timeout: timeout}
	resolved, err := resolveAssetURL(client, url, token)
	if err != nil {
		return Fetched{}, err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Fetched{}, err
	}
	sum := sha256.New()
	size, err := get(client, resolved, token, maxBytes, io.MultiWriter(f, sum), onProgress)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dest)
		return Fetched{}, err
	}
	return Fetched{SHA256: hex.EncodeToString(sum.Sum(nil)), Size: size}, nil
}

// resolveAssetURL turns a browser-style release URL into the API asset URL
// a private repository needs; any other URL is returned as it is.
func resolveAssetURL(client *http.Client, url, token string) (string, error) {
	m := browserURL.FindStringSubmatch(url)
	if m == nil {
		return url, nil
	}
	return assetAPIURL(client, m[1], m[2], m[3], m[4], token)
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

func get(client *http.Client, url, token string, maxBytes int64, w io.Writer, onProgress func(done, total int64)) (int64, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return 0, fmt.Errorf("the URL must start with http:// or https://")
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	// octet-stream is what makes the API return the asset itself rather than
	// its JSON description.
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound && token == "" {
			return 0, fmt.Errorf("the download returned 404; a private release also needs a token")
		}
		return 0, fmt.Errorf("the download returned HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		return 0, fmt.Errorf("the download returned HTML rather than a file, "+
			"which usually means an auth wall (Content-Type %q)", ct)
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("the download is %d MB, larger than the %d MB limit", resp.ContentLength>>20, maxBytes>>20)
	}
	var body io.Reader = resp.Body
	if onProgress != nil {
		onProgress(0, resp.ContentLength)
		body = &countingReader{r: resp.Body, total: resp.ContentLength, report: onProgress}
	}
	if maxBytes > 0 {
		body = io.LimitReader(body, maxBytes+1)
	}
	n, err := io.Copy(w, body)
	if err != nil {
		return n, fmt.Errorf("download: %w", err)
	}
	if maxBytes > 0 && n > maxBytes {
		return n, fmt.Errorf("the download is larger than the %d MB limit", maxBytes>>20)
	}
	return n, nil
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
