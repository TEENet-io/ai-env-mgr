package ossclient

import (
	"strings"
	"testing"
	"time"
)

// Data may move over the internal endpoint, but a presigned link must not:
// an internal host is unreachable from wherever such a link is opened, and the
// failure would be a download that simply times out for whoever was sent it.
func TestSignedURLsUseThePublicEndpoint(t *testing.T) {
	const (
		internal = "oss-ap-southeast-1-internal.aliyuncs.com"
		public   = "oss-ap-southeast-1.aliyuncs.com"
	)
	c, err := NewSplit(internal, public, "ai-collect-sg", "ak", "sk")
	if err != nil {
		t.Fatal(err)
	}
	url, err := c.SignedURL(FileKey("agent.exe"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(url, "-internal.") {
		t.Fatalf("the link points at the internal endpoint, which nobody can reach: %s", url)
	}
	if !strings.Contains(url, public) {
		t.Fatalf("the link does not use the public endpoint: %s", url)
	}
	if !strings.HasPrefix(url, "https://") {
		t.Fatalf("the link is not https: %s", url)
	}
}

// With no public endpoint given, both roles use the one endpoint -- the case
// for the agent and for an administrator's own machine.
func TestSignedURLsFallBackToTheOnlyEndpoint(t *testing.T) {
	c, err := New("oss-ap-southeast-1.aliyuncs.com", "ai-collect-sg", "ak", "sk")
	if err != nil {
		t.Fatal(err)
	}
	url, err := c.SignedURL(FileKey("agent.exe"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "oss-ap-southeast-1.aliyuncs.com") {
		t.Fatalf("unexpected host: %s", url)
	}
}
