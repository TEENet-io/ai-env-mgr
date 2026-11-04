package adminweb

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// The pages link versioned assets, which are cached for good; an unversioned
// or outdated link is still served, but revalidated.
func TestStaticAssetsAreCachedByVersion(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	h := s.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	page := get("/").Body.String()
	link := regexp.MustCompile(`href="(/static/app\.css\?v=[0-9a-f]{12})"`).FindStringSubmatch(page)
	if link == nil {
		t.Fatalf("the sign-in page links no versioned stylesheet:\n%s", page)
	}
	rec := get(link[1])
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	for _, path := range []string{"/static/app.css", "/static/app.css?v=old"} {
		rec := get(path)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d %q", path, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	// Pages themselves stay out of caches.
	if cc := get("/").Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("page Cache-Control = %q", cc)
	}
}
