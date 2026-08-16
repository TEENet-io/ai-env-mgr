package adminweb

import (
	"fmt"
	"log"
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
func downloadFromURL(url, token string) ([]byte, error) {
	return ghrelease.Fetch(url, token, 30*time.Minute) // 700 MB over a slow link
}
