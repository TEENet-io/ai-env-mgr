package ghrelease

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGitHub stands in for the API: it serves one release with one asset, and
// only to a request carrying the right token.
func fakeGitHub(t *testing.T, wantToken string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != wantToken {
			// What GitHub does with a private repository: deny that it exists.
			http.NotFound(w, r)
			return
		}
		switch {
		case r.URL.Path == "/repos/acme/tools/releases/tags/v1":
			json.NewEncoder(w).Encode(map[string]any{
				"assets": []map[string]string{
					{"name": "setup.exe", "url": srv.URL + "/repos/acme/tools/releases/assets/42"},
					{"name": "notes.txt", "url": srv.URL + "/repos/acme/tools/releases/assets/43"},
				},
			})
		case r.URL.Path == "/repos/acme/tools/releases/assets/42":
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("asset fetched with Accept %q", r.Header.Get("Accept"))
			}
			fmt.Fprint(w, "INSTALLER BYTES")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The address a person copies from the releases page cannot be fetched
// directly for a private repository -- GitHub answers 404 no matter the token.
// Resolving it through the API is what makes the obvious paste work.
func TestBrowserURLIsResolvedThroughTheAPI(t *testing.T) {
	srv := fakeGitHub(t, "good-token")
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = "https://api.github.com" })

	got, err := Fetch("https://github.com/acme/tools/releases/download/v1/setup.exe",
		"good-token", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "INSTALLER BYTES" {
		t.Fatalf("got %q", got)
	}
}

// A 404 from the API means "no such release, or your token cannot see it".
// Saying only "404" sends people to check the URL, which is usually fine.
func TestBadTokenExplainsItself(t *testing.T) {
	srv := fakeGitHub(t, "good-token")
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = "https://api.github.com" })

	_, err := Fetch("https://github.com/acme/tools/releases/download/v1/setup.exe",
		"wrong-token", 10*time.Second)
	if err == nil {
		t.Fatal("a wrong token was accepted")
	}
	if !strings.Contains(err.Error(), "Contents: Read") {
		t.Fatalf("the error does not say what to check: %v", err)
	}
}

// A typo in the file name should name the assets that do exist rather than
// leaving the operator to guess.
func TestMissingAssetListsWhatIsThere(t *testing.T) {
	srv := fakeGitHub(t, "good-token")
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = "https://api.github.com" })

	_, err := Fetch("https://github.com/acme/tools/releases/download/v1/setup.ex",
		"good-token", 10*time.Second)
	if err == nil {
		t.Fatal("a missing asset was accepted")
	}
	for _, want := range []string{"setup.exe", "notes.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not list %q: %v", want, err)
		}
	}
}

// Anything that is not a release page URL is fetched as given, so a plain file
// server or a pre-resolved API URL still works.
func TestOtherURLsAreFetchedDirectly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "PLAIN BYTES")
	}))
	defer srv.Close()

	got, err := Fetch(srv.URL+"/agent.exe", "", 10*time.Second)
	if err != nil || string(got) != "PLAIN BYTES" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

// An auth wall answers with a login page, not a binary.
func TestHTMLIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html>sign in</html>")
	}))
	defer srv.Close()

	if _, err := Fetch(srv.URL+"/x", "", 10*time.Second); err == nil ||
		!strings.Contains(err.Error(), "HTML") {
		t.Fatalf("HTML was accepted as a download: %v", err)
	}
}
