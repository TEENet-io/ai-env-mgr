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

// Nothing ties a code to this session's flow except the state, so every way of
// arriving without a matching one has to be refused. A callback that simply
// omits state is the dangerous case: it looks well-formed, and an earlier
// version let it through.
func TestCodexCallbackRequiresMatchingState(t *testing.T) {
	const cb = "http://localhost:1455/auth/callback?"
	for _, bad := range []struct{ name, pasted string }{
		{"no state at all", cb + "code=attacker"},
		{"empty state", cb + "code=attacker&state="},
		{"someone else's state", cb + "code=attacker&state=other"},
		{"a bare code", "attackercode"},
	} {
		if _, err := codexCodeFromCallback(bad.pasted, "st"); err == nil {
			t.Fatalf("accepted a callback with %s", bad.name)
		}
	}
	got, err := codexCodeFromCallback(cb+"code=goodcode&state=st", "st")
	if err != nil || got != "goodcode" {
		t.Fatalf("a matching callback failed: %q %v", got, err)
	}
	// An empty expected state must never compare equal to an absent one.
	if _, err := codexCodeFromCallback(cb+"code=c", ""); err == nil {
		t.Fatal("an empty expected state matched an absent one")
	}
}

// Claude shows "code#state" rather than redirecting, so it needs its own
// parsing -- feeding it the callback parser rejected every valid sign-in.
func TestClaudePasteRequiresMatchingState(t *testing.T) {
	got, err := claudeCodeFromPaste("goodcode#st", "st")
	if err != nil || got != "goodcode" {
		t.Fatalf("the value Claude displays was rejected: %q %v", got, err)
	}
	for _, bad := range []string{"goodcode", "goodcode#other", ""} {
		if _, err := claudeCodeFromPaste(bad, "st"); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	// A pasted URL is tolerated, but still only with a matching state.
	if _, err := claudeCodeFromPaste("https://claude.ai/cb?code=c&state=st", "st"); err != nil {
		t.Fatalf("a pasted callback URL was rejected: %v", err)
	}
	if _, err := claudeCodeFromPaste("https://claude.ai/cb?code=c", "st"); err == nil {
		t.Fatal("accepted a pasted URL with no state")
	}
	if _, err := claudeCodeFromPaste("c#", ""); err == nil {
		t.Fatal("an empty expected state matched an absent one")
	}
}
