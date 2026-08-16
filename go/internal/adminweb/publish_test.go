package adminweb

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// Publishing makes every machine download and execute a binary, so a click
// alone must not be enough: the version has to be typed back.
func TestPublishNeedsTheVersionTypedBack(t *testing.T) {
	cases := []struct{ path, key string }{
		{"/agent/publish", ossclient.AgentBinaryKey()},
		{"/codex/publish", ossclient.CodexInstallerKey("1.0.0")},
	}
	for _, c := range cases {
		fs := newFakeStore()
		s := newTestServer(t, fs)
		cookie := signIn(t, s)
		token := csrfOf(t, s, cookie)

		// No confirmation.
		rec := post(t, s, c.path, cookie, url.Values{
			"csrf": {token}, "version": {"1.0.0"}, "url": {"https://example.invalid/x"},
		})
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
			t.Fatalf("%s published without a confirmation", c.path)
		}
		if _, ok := fs.objects[c.key]; ok {
			t.Fatalf("%s uploaded without a confirmation", c.path)
		}

		// Confirmation that does not match.
		rec = post(t, s, c.path, cookie, url.Values{
			"csrf": {token}, "version": {"1.0.0"}, "confirm": {"1.0.1"},
			"url": {"https://example.invalid/x"},
		})
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
			t.Fatalf("%s accepted a mismatched confirmation", c.path)
		}
		if _, ok := fs.objects[c.key]; ok {
			t.Fatalf("%s uploaded on a mismatched confirmation", c.path)
		}
	}
}

// Forgetting a machine drops its binding and status for good.
func TestForgetNeedsTheHostnameTypedBack(t *testing.T) {
	fs := newFakeStore()
	s := newTestServer(t, fs)
	cookie := signIn(t, s)
	token := csrfOf(t, s, cookie)

	for _, confirm := range []string{"", "PC2"} {
		form := url.Values{"csrf": {token}, "machine": {"PC1"}}
		if confirm != "" {
			form.Set("confirm", confirm)
		}
		rec := post(t, s, "/machines/forget", cookie, form)
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
			t.Fatalf("forget accepted confirmation %q", confirm)
		}
	}
}

// The confirmation check itself, at the boundary: trailing whitespace from a
// copy-paste should pass, a different string should not.
func TestConfirmMatches(t *testing.T) {
	mk := func(v string) *http.Request {
		r := httptest_NewPostForm(url.Values{"confirm": {v}})
		return r
	}
	if err := confirmMatches(mk("  1.2.3  "), "confirm", "1.2.3"); err != nil {
		t.Fatalf("a pasted value with spaces was rejected: %v", err)
	}
	if err := confirmMatches(mk(""), "confirm", "1.2.3"); err == nil {
		t.Fatal("an empty confirmation passed")
	}
	if err := confirmMatches(mk("1.2.4"), "confirm", "1.2.3"); err == nil {
		t.Fatal("a mismatched confirmation passed")
	}
}

// A bare code cannot be checked against the state that ties it to this
// session's flow, so it must be refused rather than quietly accepted.
func TestCodeFromCallbackRequiresState(t *testing.T) {
	if _, err := codeFromCallback("abc123", "st"); err == nil {
		t.Fatal("a bare code was accepted with no state to verify")
	}
	if _, err := codeFromCallback("http://localhost:1455/auth/callback?code=c&state=other", "st"); err == nil {
		t.Fatal("a callback from a different flow was accepted")
	}
	got, err := codeFromCallback("http://localhost:1455/auth/callback?code=goodcode&state=st", "st")
	if err != nil || got != "goodcode" {
		t.Fatalf("a matching callback failed: %q %v", got, err)
	}
}
