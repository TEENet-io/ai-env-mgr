package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// scriptedLines returns a readLine that hands back the given answers in order.
func scriptedLines(answers ...string) readLine {
	i := 0
	return func(string) (string, error) {
		if i >= len(answers) {
			return "", io.EOF
		}
		v := answers[i]
		i++
		return v, nil
	}
}

func TestPromptCredentialsSucceedsFirstTry(t *testing.T) {
	line := scriptedLines("mybucket", "oss-cn-x.aliyuncs.com", "LTAIgood")
	secret := func(string) (string, error) { return "secretgood", nil }
	verify := func(c config.Config) error { return nil }

	cfg, err := promptCredentials(io.Discard, line, secret, verify, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bucket != "mybucket" || cfg.Endpoint != "oss-cn-x.aliyuncs.com" ||
		cfg.AccessKeyID != "LTAIgood" || cfg.AccessKeySecret != "secretgood" {
		t.Fatalf("collected config = %+v", cfg)
	}
	if cfg.IntervalMinutes != config.DefaultIntervalMinutes {
		t.Fatalf("interval = %d, want default", cfg.IntervalMinutes)
	}
}

func TestPromptCredentialsUsesPresetBucketEndpoint(t *testing.T) {
	// With bucket+endpoint preset (compiled in), only the AccessKey is asked
	// for: the line reader supplies just the key id, not bucket/endpoint.
	line := scriptedLines("LTAIgood")
	secret := func(string) (string, error) { return "secretgood", nil }
	var seen config.Config
	verify := func(c config.Config) error { seen = c; return nil }

	cfg, err := promptCredentials(io.Discard, line, secret, verify, "ai-collect-sg", "oss-ap-southeast-1.aliyuncs.com")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bucket != "ai-collect-sg" || cfg.Endpoint != "oss-ap-southeast-1.aliyuncs.com" {
		t.Fatalf("preset bucket/endpoint not used: %+v", cfg)
	}
	if cfg.AccessKeyID != "LTAIgood" || cfg.AccessKeySecret != "secretgood" {
		t.Fatalf("AccessKey not collected: %+v", cfg)
	}
	if seen.Bucket != "ai-collect-sg" {
		t.Fatalf("verify saw bucket %q, want the preset", seen.Bucket)
	}
}

func TestPromptCredentialsRetriesWrongKeyThenSucceeds(t *testing.T) {
	// bucket, endpoint, then key id #1 (bad), key id #2 (good).
	line := scriptedLines("mybucket", "oss-cn-x.aliyuncs.com", "LTAIbad", "LTAIgood")
	secrets := scriptedLines("badsecret", "goodsecret")
	secret := readSecret(secrets)

	calls := 0
	verify := func(c config.Config) error {
		calls++
		if c.AccessKeyID == "LTAIgood" {
			return nil
		}
		return fmt.Errorf("%w (InvalidAccessKeyId)", ossclient.ErrBadCredentials)
	}

	cfg, err := promptCredentials(io.Discard, line, secret, verify, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessKeyID != "LTAIgood" || cfg.AccessKeySecret != "goodsecret" {
		t.Fatalf("expected the second key pair, got %+v", cfg)
	}
	if calls != 2 {
		t.Fatalf("verify called %d times, want 2 (one bad, one good)", calls)
	}
}

func TestPromptCredentialsAccessDeniedDoesNotRetry(t *testing.T) {
	line := scriptedLines("mybucket", "oss-cn-x.aliyuncs.com", "LTAIvalid")
	secret := func(string) (string, error) { return "secret", nil }
	calls := 0
	verify := func(c config.Config) error {
		calls++
		return fmt.Errorf("%w (AccessDenied)", ossclient.ErrAccessDenied)
	}

	_, err := promptCredentials(io.Discard, line, secret, verify, "", "")
	if err == nil {
		t.Fatal("expected an error for access denied")
	}
	if !errors.Is(err, ossclient.ErrAccessDenied) {
		t.Fatalf("error = %v, want ErrAccessDenied", err)
	}
	if calls != 1 {
		t.Fatalf("verify called %d times, want 1 (no retry on access denied)", calls)
	}
	if !strings.Contains(err.Error(), "RAM policy") {
		t.Fatalf("error should hint at the policy fix: %v", err)
	}
}

func TestPromptCredentialsGivesUpAfterMaxAttempts(t *testing.T) {
	// Enough key answers for maxCredAttempts tries.
	answers := []string{"mybucket", "oss-cn-x.aliyuncs.com"}
	for i := 0; i < maxCredAttempts; i++ {
		answers = append(answers, fmt.Sprintf("LTAIbad%d", i))
	}
	line := scriptedLines(answers...)
	secret := func(string) (string, error) { return "badsecret", nil }
	verify := func(c config.Config) error {
		return fmt.Errorf("%w (InvalidAccessKeyId)", ossclient.ErrBadCredentials)
	}

	_, err := promptCredentials(io.Discard, line, secret, verify, "", "")
	if err == nil {
		t.Fatal("expected failure after exhausting attempts")
	}
	if !errors.Is(err, ossclient.ErrBadCredentials) {
		t.Fatalf("error = %v, want ErrBadCredentials", err)
	}
}
