package slsclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testAK   = "testid"
	testSK   = "testsecret"
	testDate = "Mon, 02 Jan 2006 15:04:05 GMT"
)

// The sign string is the whole of the authentication, and it is the one thing
// a stand-in server cannot check for us: a wrong one fails only against the
// real service, in production, as a 403 nobody can explain. So it is asserted
// here as exact text, written out by hand from the SDK's rules rather than
// produced by the code under test.
func TestStringToSignMatchesTheSDK(t *testing.T) {
	const uri = "/logstores/audit?from=1&line=10&offset=0&query=event_type%3A+llm_call&reverse=true&to=2&type=log"
	headers := map[string]string{
		"Date":                  testDate,
		"Host":                  "windows-control-logs.ap-southeast-1.log.aliyuncs.com",
		"User-Agent":            userAgent,
		"Accept":                "application/json",
		"x-log-apiversion":      apiVersion,
		"x-log-bodyrawsize":     "0",
		"x-log-signaturemethod": signatureMethod,
	}
	// GET with no body: Content-MD5 and Content-Type are empty, so lines two
	// and three are blank. The x-log- headers are sorted; the query keys are
	// sorted too but carry DECODED values, which is why "%3A+" appears here
	// as ": ".
	want := "GET\n" +
		"\n" +
		"\n" +
		testDate + "\n" +
		"x-log-apiversion:0.6.0\n" +
		"x-log-bodyrawsize:0\n" +
		"x-log-signaturemethod:hmac-sha1\n" +
		"/logstores/audit?from=1&line=10&offset=0&query=event_type: llm_call&reverse=true&to=2&type=log"

	got, err := stringToSign(http.MethodGet, uri, headers)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("string to sign mismatch\n got: %q\nwant: %q", got, want)
	}

	// And the header the sign string ends up in.
	c := New("ap-southeast-1.log.aliyuncs.com", "windows-control-logs", testAK, testSK)
	signed := map[string]string{
		"Date":              testDate,
		"x-log-apiversion":  apiVersion,
		"x-log-bodyrawsize": "0",
	}
	if err := c.sign(http.MethodGet, uri, signed); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha1.New, []byte(testSK))
	mac.Write([]byte(want))
	wantAuth := "LOG " + testAK + ":" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if signed["Authorization"] != wantAuth {
		t.Fatalf("Authorization = %q, want %q", signed["Authorization"], wantAuth)
	}
	if signed["x-log-signaturemethod"] != signatureMethod {
		t.Fatalf("sign did not set x-log-signaturemethod: %q", signed["x-log-signaturemethod"])
	}
}

// A Date header is not optional: the SDK refuses without one, and so does SLS.
func TestStringToSignNeedsDate(t *testing.T) {
	if _, err := stringToSign(http.MethodGet, "/logstores/audit", map[string]string{}); err == nil {
		t.Fatal("expected an error with no Date header")
	}
}

func TestGetLogsSendsAndParses(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("x-log-count", "2")
		w.Header().Set("x-log-progress", "Complete")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
		  {"event_id":"e1","status":"success","employee_id":"peter","total_tokens":"1200"},
		  {"event_id":"e2","status":"failure","employee_id":"work1","error_class":"rate_limit"}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "windows-control-logs", testAK, testSK)
	res, err := c.GetLogs(context.Background(), "audit", "event_type: llm_call", 1, 2, 10, 0, true)
	if err != nil {
		t.Fatal(err)
	}

	if got.URL.Path != "/logstores/audit" {
		t.Errorf("path = %q", got.URL.Path)
	}
	q := got.URL.Query()
	for k, want := range map[string]string{
		"type": "log", "from": "1", "to": "2", "line": "10", "offset": "0",
		"reverse": "true", "query": "event_type: llm_call",
	} {
		if q.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), want)
		}
	}
	// Host is what the server saw, not a header net/http would drop.
	if got.Host != strings.TrimPrefix(srv.URL, "http://") {
		t.Errorf("Host = %q, want %q", got.Host, strings.TrimPrefix(srv.URL, "http://"))
	}
	if got.Header.Get("Date") == "" {
		t.Error("no Date header")
	}
	if !strings.HasPrefix(got.Header.Get("Authorization"), "LOG "+testAK+":") {
		t.Errorf("Authorization = %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("x-log-apiversion") != apiVersion {
		t.Errorf("x-log-apiversion = %q", got.Header.Get("x-log-apiversion"))
	}
	if got.Header.Get("x-log-bodyrawsize") != "0" {
		t.Errorf("x-log-bodyrawsize = %q", got.Header.Get("x-log-bodyrawsize"))
	}
	if got.Header.Get("User-Agent") != userAgent {
		t.Errorf("User-Agent = %q", got.Header.Get("User-Agent"))
	}

	if !res.Complete || len(res.Logs) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if res.Logs[0]["employee_id"] != "peter" || res.Logs[1]["error_class"] != "rate_limit" {
		t.Fatalf("rows = %+v", res.Logs)
	}
	// An absent optional field is absent, not "".
	if _, ok := res.Logs[0]["error_class"]; ok {
		t.Error("row 0 invented an error_class")
	}
}

// An index still catching up answers Incomplete; the caller has to be able to
// see that, because the rows are then a partial answer presented as a whole.
func TestGetLogsReportsIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-log-count", "0")
		w.Header().Set("x-log-progress", "Incomplete")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	res, err := New(srv.URL, "p", testAK, testSK).
		GetLogs(context.Background(), "audit", "*", 1, 2, 10, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Fatal("Incomplete was reported as complete")
	}
}

func TestGetLogsForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errorCode":"Unauthorized","errorMessage":"denied by policy"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "p", testAK, testSK).
		GetLogs(context.Background(), "audit", "*", 1, 2, 10, 0, true)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("error dropped the errorCode: %v", err)
	}
}

func TestGetLogsServerErrorCarriesTheCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errorCode":"InternalServerError","errorMessage":"try again"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "p", testAK, testSK).
		GetLogs(context.Background(), "audit", "*", 1, 2, 10, 0, true)
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatal("a 500 must not read as a permission problem")
	}
	if !strings.Contains(err.Error(), "InternalServerError") {
		t.Errorf("error dropped the errorCode: %v", err)
	}
}

// A hung SLS must surface as the caller's own deadline, so the page can time
// out rather than hold a request open.
func TestGetLogsHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := New(srv.URL, "p", testAK, testSK).
		GetLogs(ctx, "audit", "*", 1, 2, 10, 0, true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// A body that is not the documented shape is an error, not a blank page that
// silently claims there were no calls.
func TestGetLogsRejectsGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "p", testAK, testSK).
		GetLogs(context.Background(), "audit", "*", 1, 2, 10, 0, true); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestNewDefaultsTheEndpoint(t *testing.T) {
	c := New("", "windows-control-logs", testAK, testSK)
	origin, host, err := c.origin()
	if err != nil {
		t.Fatal(err)
	}
	if host != "windows-control-logs."+DefaultEndpoint {
		t.Errorf("host = %q", host)
	}
	if origin != "https://"+host {
		t.Errorf("origin = %q", origin)
	}
}

// The Authorization header is an HMAC over the AccessKey secret: anyone who
// can read it off the wire can replay it against the real logstore. The
// plaintext endpoint form is a test affordance, so it must not be usable to
// reach anything but this machine -- a typo in --sls-endpoint must fail, not
// quietly ship a signed request across a network.
func TestPlaintextEndpointIsLoopbackOnly(t *testing.T) {
	for _, ep := range []string{
		"http://collector.example:8080",
		"http://10.0.0.5:8080",
		"http://ap-southeast-1.log.aliyuncs.com",
		// userinfo: a hand-rolled host split reads a loopback host, the
		// dialer connects to evil.example.
		"http://127.0.0.1:80@evil.example",
		"http://localhost:80@evil.example",
		"http://[::1]:80@evil.example",
		"http://127.0.0.1.evil.example",
		"http://localhost.evil",
		"http://127.0.0.1/evil",
	} {
		c := New(ep, "windows-control-logs", testAK, testSK)
		origin, _, err := c.origin()
		if err == nil {
			t.Errorf("%s was accepted, origin = %q", ep, origin)
			continue
		}
		// And the refusal reaches the caller rather than being swallowed.
		if _, gerr := c.GetLogs(context.Background(), "audit", "*", 1, 2, 10, 0, true); gerr == nil {
			t.Errorf("%s: GetLogs made a request anyway", ep)
		}
	}

	// Loopback in every spelling still works, or the test servers below
	// could not be reached.
	for _, ep := range []string{
		"http://127.0.0.1:9999", "http://localhost:9999", "http://[::1]:9999",
	} {
		if _, _, err := New(ep, "p", testAK, testSK).origin(); err != nil {
			t.Errorf("%s was rejected: %v", ep, err)
		}
	}
}

// An error body that is not the documented JSON -- an HTML page from a proxy
// in front of SLS, say -- must not arrive whole in a notice or a log line.
func TestErrorBodyIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>" + strings.Repeat("x", 5000) + "</html>"))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "p", testAK, testSK).
		GetLogs(context.Background(), "audit", "*", 1, 2, 10, 0, true)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > 300 {
		t.Fatalf("error is %d bytes, want it truncated: %.120q…", len(err.Error()), err.Error())
	}
}

// Code is how the page tells a missing policy from a signing bug: both are
// ErrForbidden, and only one is about permissions.
func TestCodeSurvivesTheForbiddenWrap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errorCode":"SignatureNotMatch","errorMessage":"bad signature"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "p", testAK, testSK).
		GetLogs(context.Background(), "audit", "*", 1, 2, 10, 0, true)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if got := Code(err); got != "SignatureNotMatch" {
		t.Fatalf("Code(err) = %q, want SignatureNotMatch", got)
	}
	if Code(errors.New("plain")) != "" {
		t.Error("Code invented a code for a plain error")
	}
}
