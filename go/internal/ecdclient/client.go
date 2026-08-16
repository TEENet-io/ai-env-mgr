// Package ecdclient reads cloud desktop state from Alibaba Cloud's WuYing
// (Elastic Desktop Service) API.
//
// It exists to answer one question the agent cannot: whether a machine that
// has gone quiet is hibernating or has actually failed. WuYing suspends a
// desktop at the platform level, so the guest Windows never receives a power
// event and the agent gets no chance to say it is going away -- verified on a
// real desktop, where a suspend produced neither a lifecycle log line nor a
// wake-up sync. The only source that knows is the platform itself.
//
// Only DescribeDesktops is implemented, and the signing is done here rather
// than by pulling in alibaba-cloud-sdk-go: one read-only call does not justify
// a dependency of that size, and V3 signing is a few dozen lines of standard
// library.
package ecdclient

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Client talks to one region's ECD endpoint.
type Client struct {
	accessKeyID     string
	accessKeySecret string
	endpoint        string // ecd.<region>.aliyuncs.com
	region          string
	http            *http.Client
}

// New builds a client for one region.
func New(accessKeyID, accessKeySecret, region string) (*Client, error) {
	switch {
	case accessKeyID == "":
		return nil, fmt.Errorf("access key id is required")
	case accessKeySecret == "":
		return nil, fmt.Errorf("access key secret is required")
	case region == "":
		return nil, fmt.Errorf("region is required")
	}
	return &Client{
		accessKeyID:     accessKeyID,
		accessKeySecret: accessKeySecret,
		endpoint:        "ecd." + region + ".aliyuncs.com",
		region:          region,
		http:            &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Desktop is the part of a cloud desktop this project uses.
type Desktop struct {
	DesktopID   string `json:"DesktopId"`
	DesktopName string `json:"DesktopName"`
	// HostName is the guest's Windows computer name, which is how a desktop is
	// matched to an agent's report. Windows caps that at 15 characters while a
	// DesktopId is longer, so the two are not the same string.
	HostName string `json:"HostName"`
	// Status is the power state: Running, Stopped, Starting, Stopping,
	// Rebuilding, Expired, Deleted, Pending.
	Status string `json:"DesktopStatus"`
	// ManagementFlags is where hibernation lives -- NOT DesktopStatus, which
	// has no value for it. Reading only the status would collapse "asleep" and
	// "shut down" into one, which is exactly the distinction this package
	// exists to make.
	ManagementFlags []string `json:"ManagementFlags"`
	ConnectionState string   `json:"ConnectionStatus"`
	NetworkIP       string   `json:"NetworkInterfaceIp"`
	EndUserIDs      []string `json:"EndUserIds"`
	OfficeSiteID    string   `json:"OfficeSiteId"`
	OfficeSiteName  string   `json:"OfficeSiteName"`
}

// Hibernated reports whether the platform is holding this desktop suspended.
func (d Desktop) Hibernated() bool {
	for _, f := range d.ManagementFlags {
		switch f {
		case "Hibernated", "Hibernating":
			return true
		}
	}
	return false
}

// DescribeDesktops lists every desktop in the region, following pagination.
func (c *Client) DescribeDesktops() ([]Desktop, error) {
	var all []Desktop
	next := ""
	for {
		q := url.Values{
			"RegionId":   {c.region},
			"MaxResults": {"100"},
		}
		if next != "" {
			q.Set("NextToken", next)
		}
		body, err := c.call("DescribeDesktops", q)
		if err != nil {
			return nil, err
		}
		var page struct {
			Desktops  []Desktop `json:"Desktops"`
			NextToken string    `json:"NextToken"`
			Code      string    `json:"Code"`
			Message   string    `json:"Message"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode DescribeDesktops: %w", err)
		}
		if page.Code != "" {
			return nil, fmt.Errorf("DescribeDesktops: %s: %s", page.Code, page.Message)
		}
		all = append(all, page.Desktops...)
		if page.NextToken == "" {
			return all, nil
		}
		next = page.NextToken
		if len(all) > 10000 { // a fleet this size means something is wrong
			return all, fmt.Errorf("DescribeDesktops returned more than 10000 desktops; stopping")
		}
	}
}

// call performs one signed RPC request.
func (c *Client) call(action string, query url.Values) ([]byte, error) {
	const emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"host":                  c.endpoint,
		"x-acs-action":          action,
		"x-acs-version":         "2020-09-30",
		"x-acs-date":            time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"x-acs-signature-nonce": nonce,
		"x-acs-content-sha256":  emptyBodySHA256,
	}

	canonicalQuery := canonicalQueryString(query)
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, k := range names {
		canonicalHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		"/", // RPC style always addresses the root
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders,
		emptyBodySHA256,
	}, "\n")

	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "ACS3-HMAC-SHA256\n" + hex.EncodeToString(sum[:])
	mac := hmac.New(sha256.New, []byte(c.accessKeySecret))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest(http.MethodGet, "https://"+c.endpoint+"/?"+canonicalQuery, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		if k == "host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		"ACS3-HMAC-SHA256 Credential=%s,SignedHeaders=%s,Signature=%s",
		c.accessKeyID, signedHeaders, signature))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", action, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// The body carries Code/Message, which say far more than the status.
		var e struct{ Code, Message string }
		if json.Unmarshal(body, &e) == nil && e.Code != "" {
			return nil, fmt.Errorf("%s returned HTTP %d: %s: %s", action, resp.StatusCode, e.Code, e.Message)
		}
		return nil, fmt.Errorf("%s returned HTTP %d", action, resp.StatusCode)
	}
	return body, nil
}

// canonicalQueryString sorts and percent-encodes the query the way V3 signing
// requires: RFC3986, so a space is %20 rather than +, and ~ is left alone.
func canonicalQueryString(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, percentEncode(k)+"="+percentEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func percentEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	e = strings.ReplaceAll(e, "%7E", "~")
	return e
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
