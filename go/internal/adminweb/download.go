package adminweb

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// logAudit records an action that changed what the fleet does.
//
// These lines are the record of who published what, so they carry the client
// address (see clientKey, which resolves the real one behind the proxy) and
// deliberately never the credentials or token involved.
func logAudit(client, format string, args ...any) {
	log.Printf("adminweb: [audit] %s %s", client, fmt.Sprintf(format, args...))
}

// downloadFromURL fetches a payload the server publishes on the operator's
// behalf, mirroring `admin agent publish --url`.
//
// Fetching server-side rather than uploading through the browser is the only
// practical route for the Codex installer: it is ~700 MB, and a CDN in front
// of the console will usually refuse a body that size long before it arrives.
//
// A PRIVATE GitHub release asset needs a token, or GitHub answers 404 (it
// hides private repositories) or an HTML sign-in page. A cross-host redirect
// to the signed asset URL is followed by the default client, which strips the
// Authorization header on the way, as it should.
func downloadFromURL(url, token string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return nil, fmt.Errorf("the URL must start with http:// or https://")
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Minute} // 700 MB over a slow link
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound && token == "" {
			return nil, fmt.Errorf("the download returned 404; a private GitHub release also needs a token")
		}
		return nil, fmt.Errorf("the download returned HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		return nil, fmt.Errorf("the download returned HTML rather than a file, which usually means an auth wall; pass a token")
	}
	return io.ReadAll(resp.Body)
}
