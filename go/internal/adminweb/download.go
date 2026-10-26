package adminweb

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ghrelease"
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
func downloadFromURL(url, token string, onProgress func(done, total int64)) ([]byte, error) {
	if err := validatePayloadURL(url); err != nil {
		return nil, err
	}
	// Installers are large, but an administrator should never be able to make
	// the console buffer an unbounded response.  The fetcher streams to a
	// temporary file before returning bytes; 2 GiB leaves room for Codex while
	// still putting a hard ceiling on accidental or malicious downloads.
	return ghrelease.FetchProgressLimited(url, token, 30*time.Minute, 2<<30, onProgress)
}

func validatePayloadURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return fmt.Errorf("download URL must include a host")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("download URL must use https")
	}
	// HTTP is accepted only for loopback test/dev endpoints. Production URLs
	// must be HTTPS so a redirected installer cannot be tampered with in
	// transit.
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("download URL must use https (http is allowed only for localhost)")
	}
	if ip != nil && !ip.IsLoopback() && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return fmt.Errorf("download URL points to a private or local address")
	}
	return nil
}
