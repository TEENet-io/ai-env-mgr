package adminweb

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
)

// httptest_NewPostForm builds a parsed POST form request for unit-level checks.
func httptest_NewPostForm(form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = r.ParseForm()
	return r
}
