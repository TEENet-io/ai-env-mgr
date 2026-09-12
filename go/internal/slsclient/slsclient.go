// Package slsclient is a minimal read-only client for Alibaba Cloud Simple
// Log Service (SLS), just large enough for the console's log page.
//
// The official SDK is not used on purpose. It would pull in a dependency tree
// (backoff, go-kit, protobuf, lz4) for one GET, and this console ships as a
// single Windows binary whose supply chain is meant to stay small. What is
// here is the v1 ("LOG" prefix) signature and one call, GetLogs.
//
// Like every other credential in this console, the AccessKey lives only in
// the Client's memory: it is handed over at sign-in, never written down and
// never logged.
package slsclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is where this deployment's project lives. Callers that have
// no configured endpoint pass "" and get this.
const DefaultEndpoint = "ap-southeast-1.log.aliyuncs.com"

const (
	apiVersion      = "0.6.0"
	signatureMethod = "hmac-sha1"
	userAgent       = "ai-env-mgr-admin"

	// requestTimeout bounds a single call. A page runs a handful of queries
	// in series, so this has to be short enough that their sum still leaves
	// the console responsive.
	requestTimeout = 15 * time.Second

	// maxBody caps what is read back. A logstore query asking for 100 lines
	// cannot legitimately come anywhere near this; the cap is here so a
	// surprising response cannot exhaust the console's memory.
	maxBody = 32 << 20
)

// ErrForbidden reports that the credentials reached SLS but are not allowed
// to read it -- an AccessKey without AliyunLogReadOnlyAccess, which is the
// one failure the page can give the operator a concrete fix for.
var ErrForbidden = errors.New("sls: access denied")

// Client reads one SLS project.
type Client struct {
	endpoint string
	project  string
	ak       string
	sk       string
	http     *http.Client
}

// New builds a client. An empty endpoint means DefaultEndpoint.
//
// An endpoint beginning with "http://" is taken literally -- host and all,
// with no project prefix -- which is how tests point the client at a local
// stand-in. Real endpoints are bare hostnames and are reached over TLS as
// "https://{project}.{endpoint}".
func New(endpoint, project, accessKeyID, accessKeySecret string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		endpoint: endpoint,
		project:  project,
		ak:       accessKeyID,
		sk:       accessKeySecret,
		http:     &http.Client{Timeout: requestTimeout},
	}
}

// Project is the project this client reads, for display.
func (c *Client) Project() string { return c.project }

// origin returns the scheme+host to send to and the Host header to sign with.
func (c *Client) origin() (string, string) {
	if rest, ok := strings.CutPrefix(c.endpoint, "http://"); ok {
		return "http://" + rest, rest
	}
	host := c.project + "." + strings.TrimPrefix(c.endpoint, "https://")
	return "https://" + host, host
}

// Log is one row: SLS answers with string values throughout, for raw log
// lines and for SQL result columns alike. Optional fields that were empty at
// write time are absent rather than present and empty.
type Log map[string]string

// Result is one page of a query.
type Result struct {
	Logs []Log
	// Count is what x-log-count reported: the rows in this response.
	Count int
	// Complete is x-log-progress == "Complete". False means the index was
	// still catching up and the rows below may be missing some.
	Complete bool
}

// GetLogs runs one query over [from, to] (unix seconds, inclusive) and returns
// up to line rows from offset, newest first when reverse is true.
//
// query is the SLS search expression, optionally followed by "| <SQL>" for an
// aggregate; an aggregate answers one row per group, with the SELECT aliases
// as keys.
func (c *Client) GetLogs(ctx context.Context, logstore, query string, from, to int64, line, offset int, reverse bool) (*Result, error) {
	v := url.Values{}
	v.Set("type", "log")
	v.Set("from", strconv.FormatInt(from, 10))
	v.Set("to", strconv.FormatInt(to, 10))
	v.Set("query", query)
	v.Set("line", strconv.Itoa(line))
	v.Set("offset", strconv.Itoa(offset))
	v.Set("reverse", strconv.FormatBool(reverse))
	// Encode sorts by key and percent-encodes, which is also the order the
	// signature's CanonicalizedResource wants -- see stringToSign.
	uri := "/logstores/" + logstore + "?" + v.Encode()

	origin, host := c.origin()
	headers := map[string]string{
		"Date":              nowRFC1123(),
		"Host":              host,
		"User-Agent":        userAgent,
		"Accept":            "application/json",
		"x-log-apiversion":  apiVersion,
		"x-log-bodyrawsize": "0",
	}
	if err := c.sign(http.MethodGet, uri, headers); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+uri, nil)
	if err != nil {
		return nil, err
	}
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Returned unwrapped so callers can still see a context deadline or
		// cancellation through errors.Is.
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("sls: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(resp.StatusCode, body)
	}

	logs, err := decodeLogs(body)
	if err != nil {
		return nil, err
	}
	res := &Result{
		Logs:     logs,
		Complete: resp.Header.Get("x-log-progress") == "Complete",
	}
	// A missing or unparsable count is not worth failing the page over; the
	// rows themselves are the answer.
	if n, cerr := strconv.Atoi(resp.Header.Get("x-log-count")); cerr == nil {
		res.Count = n
	} else {
		res.Count = len(logs)
	}
	return res, nil
}

// decodeLogs turns the JSON array into rows.
//
// Every value is expected to be a JSON string, but a non-string is kept as
// its raw JSON rather than failing the whole page: one unexpected column
// should not hide ninety-nine good rows.
func decodeLogs(body []byte) ([]Log, error) {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("sls: parse response: %w", err)
	}
	logs := make([]Log, 0, len(raw))
	for _, row := range raw {
		l := make(Log, len(row))
		for k, v := range row {
			var s string
			if json.Unmarshal(v, &s) == nil {
				l[k] = s
			} else {
				l[k] = string(v)
			}
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// slsError is a non-2xx answer from SLS, carrying the code it named.
type slsError struct {
	HTTPCode int
	Code     string
	Message  string
}

func (e *slsError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("sls: HTTP %d: %s", e.HTTPCode, e.Message)
	}
	return fmt.Sprintf("sls: %s: %s", e.Code, e.Message)
}

// apiError builds the error for a non-2xx response. 401 and 403 additionally
// wrap ErrForbidden, which is the one case the page can act on.
func apiError(code int, body []byte) error {
	e := &slsError{HTTPCode: code}
	var parsed struct {
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		e.Code, e.Message = parsed.ErrorCode, parsed.ErrorMessage
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Errorf("%w (%s)", ErrForbidden, e.Error())
	}
	return e
}

// gmt is the zone the Date header must be in. Built rather than loaded so it
// works on a Windows host with no tzdata.
var gmt = time.FixedZone("GMT", 0)

func nowRFC1123() string { return time.Now().In(gmt).Format(time.RFC1123) }

// sign adds x-log-signaturemethod and Authorization, following the official
// Go SDK's SignerV1.Sign.
func (c *Client) sign(method, uri string, headers map[string]string) error {
	headers["x-log-signaturemethod"] = signatureMethod
	s, err := stringToSign(method, uri, headers)
	if err != nil {
		return err
	}
	mac := hmac.New(sha1.New, []byte(c.sk))
	mac.Write([]byte(s))
	headers["Authorization"] = "LOG " + c.ak + ":" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return nil
}

// stringToSign builds the v1 sign string:
//
//	VERB\nContent-MD5\nContent-Type\nDate\nCanonicalizedSLSHeaders\nCanonicalizedResource
//
// Only GET with no body is used here, so Content-MD5 and Content-Type are
// always empty, leaving two blank lines after the verb.
//
// Two details are easy to get wrong and are taken from the SDK rather than
// from the prose documentation: the x-log-/x-acs- headers are lowercased,
// trimmed and sorted with no trailing newline of their own, and the query
// parameters in CanonicalizedResource are sorted by key but written with
// their DECODED values -- so "query=a%3A+b" signs as "query=a: b".
func stringToSign(method, uri string, headers map[string]string) (string, error) {
	var contentMD5, contentType string
	if v, ok := headers["Content-MD5"]; ok {
		contentMD5 = v
	}
	if v, ok := headers["Content-Type"]; ok {
		contentType = v
	}
	date, ok := headers["Date"]
	if !ok {
		return "", errors.New("sls: missing Date header")
	}

	slsHeaders := make(map[string]string, len(headers))
	keys := make([]string, 0, len(headers))
	for k, v := range headers {
		l := strings.TrimSpace(strings.ToLower(k))
		if strings.HasPrefix(l, "x-log-") || strings.HasPrefix(l, "x-acs-") {
			slsHeaders[l] = strings.TrimSpace(v)
			keys = append(keys, l)
		}
	}
	sort.Strings(keys)
	var canoHeaders strings.Builder
	for i, k := range keys {
		if i > 0 {
			canoHeaders.WriteString("\n")
		}
		canoHeaders.WriteString(k + ":" + slsHeaders[k])
	}

	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	var canoResource strings.Builder
	canoResource.WriteString(u.EscapedPath())
	if u.RawQuery != "" {
		vals := u.Query()
		qkeys := make([]string, 0, len(vals))
		for k := range vals {
			qkeys = append(qkeys, k)
		}
		sort.Strings(qkeys)
		canoResource.WriteString("?")
		for i, k := range qkeys {
			if i > 0 {
				canoResource.WriteString("&")
			}
			for _, v := range vals[k] {
				canoResource.WriteString(k + "=" + v)
			}
		}
	}

	return method + "\n" +
		contentMD5 + "\n" +
		contentType + "\n" +
		date + "\n" +
		canoHeaders.String() + "\n" +
		canoResource.String(), nil
}
