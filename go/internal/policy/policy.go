// Package policy turns a model.Policy's blocked-domain list into the
// browser-vendor group-policy registry state that actually keeps a user out
// of a site.
//
// The values are written under HKEY_LOCAL_MACHINE rather than
// HKEY_CURRENT_USER. HKLM requires administrative rights to modify, so a
// standard-user employee account cannot edit or delete the block list to
// unblock a site themselves; only the provisioning/admin tooling running
// with elevated rights can.
//
// This file contains the platform-independent parts: the registry paths and
// the pure functions that turn a domain list into the per-browser value
// lists. It has no dependency on golang.org/x/sys/windows/registry, so it
// builds and is testable on any OS. The actual registry I/O lives in
// registry_windows.go (behind a "windows" build tag) and its non-Windows
// stub in registry_stub.go.
package policy

import "fmt"

// Registry key paths (relative to HKLM) for each browser's blocklist policy.
// Chrome and Edge share the same Chromium policy schema; Firefox uses its
// own enterprise policy engine with a different value format.
const (
	ChromeKey  = `SOFTWARE\Policies\Google\Chrome\URLBlocklist`
	EdgeKey    = `SOFTWARE\Policies\Microsoft\Edge\URLBlocklist`
	FirefoxKey = `SOFTWARE\Policies\Mozilla\Firefox\WebsiteFilter\Block`
)

// ChromiumEntries returns the ordered list of URLBlocklist values to write
// for Chrome/Edge. Chromium's URLBlocklist policy matches a bare domain
// against the whole origin (all subdomains and paths), so no expansion is
// needed here: each domain becomes exactly one value.
func ChromiumEntries(domains []string) []string {
	entries := make([]string, 0, len(domains))
	entries = append(entries, domains...)
	return entries
}

// FirefoxEntries returns the ordered list of WebsiteFilter/Block values to
// write for Firefox. Unlike Chromium, Firefox's WebsiteFilter policy has no
// implicit subdomain match: a bare domain pattern only blocks that exact
// host. So each domain is expanded into two wildcard patterns, one for the
// bare host and one for every subdomain:
//
//	*://<domain>/*      (the domain itself, any scheme/path)
//	*://*.<domain>/*     (any subdomain of the domain, any scheme/path)
func FirefoxEntries(domains []string) []string {
	entries := make([]string, 0, len(domains)*2)
	for _, d := range domains {
		entries = append(entries, fmt.Sprintf("*://%s/*", d), fmt.Sprintf("*://*.%s/*", d))
	}
	return entries
}
