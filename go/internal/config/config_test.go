package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

func TestLoadValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "agent.config.json", `{
		"bucket": "ai-sandbox-bucket",
		"endpoint": "oss-cn-hangzhou.aliyuncs.com",
		"accessKeyId": "LTAI-example-id",
		"accessKeySecret": "example-secret",
		"intervalMinutes": 15
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Bucket != "ai-sandbox-bucket" {
		t.Errorf("Bucket = %q, want %q", cfg.Bucket, "ai-sandbox-bucket")
	}
	if cfg.Endpoint != "oss-cn-hangzhou.aliyuncs.com" {
		t.Errorf("Endpoint = %q, want %q", cfg.Endpoint, "oss-cn-hangzhou.aliyuncs.com")
	}
	if cfg.AccessKeyID != "LTAI-example-id" {
		t.Errorf("AccessKeyID = %q, want %q", cfg.AccessKeyID, "LTAI-example-id")
	}
	if cfg.AccessKeySecret != "example-secret" {
		t.Errorf("AccessKeySecret = %q, want %q", cfg.AccessKeySecret, "example-secret")
	}
	if cfg.IntervalMinutes != 15 {
		t.Errorf("IntervalMinutes = %d, want %d", cfg.IntervalMinutes, 15)
	}
}

func TestLoadMissingIntervalDefaultsTo30(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "admin.config.json", `{
		"bucket": "ai-sandbox-bucket",
		"endpoint": "oss-cn-hangzhou.aliyuncs.com",
		"accessKeyId": "LTAI-example-id",
		"accessKeySecret": "example-secret"
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.IntervalMinutes != 30 {
		t.Errorf("IntervalMinutes = %d, want default 30", cfg.IntervalMinutes)
	}
}

func TestLoadZeroOrNegativeIntervalDefaultsTo30(t *testing.T) {
	dir := t.TempDir()

	zeroPath := writeConfig(t, dir, "zero.config.json", `{
		"bucket": "b", "endpoint": "e", "accessKeyId": "id", "accessKeySecret": "secret",
		"intervalMinutes": 0
	}`)
	cfg, err := Load(zeroPath)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.IntervalMinutes != 30 {
		t.Errorf("IntervalMinutes = %d, want default 30 for zero input", cfg.IntervalMinutes)
	}

	negPath := writeConfig(t, dir, "neg.config.json", `{
		"bucket": "b", "endpoint": "e", "accessKeyId": "id", "accessKeySecret": "secret",
		"intervalMinutes": -5
	}`)
	cfg, err = Load(negPath)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.IntervalMinutes != 30 {
		t.Errorf("IntervalMinutes = %d, want default 30 for negative input", cfg.IntervalMinutes)
	}
}

func TestLoadMissingBucketReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "agent.config.json", `{
		"endpoint": "oss-cn-hangzhou.aliyuncs.com",
		"accessKeyId": "id",
		"accessKeySecret": "secret"
	}`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error for missing bucket")
	}
	if !strings.Contains(err.Error(), "bucket") {
		t.Errorf("error %q should mention missing field %q", err.Error(), "bucket")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should mention file path %q", err.Error(), path)
	}
}

func TestLoadMissingEndpointReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "agent.config.json", `{
		"bucket": "ai-sandbox-bucket",
		"accessKeyId": "id",
		"accessKeySecret": "secret"
	}`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error for missing endpoint")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("error %q should mention missing field %q", err.Error(), "endpoint")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should mention file path %q", err.Error(), path)
	}
}

func TestLoadMissingAccessKeyIDReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "agent.config.json", `{
		"bucket": "ai-sandbox-bucket",
		"endpoint": "oss-cn-hangzhou.aliyuncs.com",
		"accessKeySecret": "secret"
	}`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error for missing accessKeyId")
	}
	if !strings.Contains(err.Error(), "accessKeyId") {
		t.Errorf("error %q should mention missing field %q", err.Error(), "accessKeyId")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should mention file path %q", err.Error(), path)
	}
}

func TestLoadMissingAccessKeySecretReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "agent.config.json", `{
		"bucket": "ai-sandbox-bucket",
		"endpoint": "oss-cn-hangzhou.aliyuncs.com",
		"accessKeyId": "id"
	}`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error for missing accessKeySecret")
	}
	if !strings.Contains(err.Error(), "accessKeySecret") {
		t.Errorf("error %q should mention missing field %q", err.Error(), "accessKeySecret")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should mention file path %q", err.Error(), path)
	}
}

func TestLoadFileNotFoundReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.config.json")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error for missing file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should mention file path %q", err.Error(), path)
	}
}

func TestLoadInvalidJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "broken.config.json", `{ this is not valid json `)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error for invalid JSON")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should mention file path %q", err.Error(), path)
	}
}

func TestDefaultAgentPathEndsWithAgentConfigJSON(t *testing.T) {
	path := DefaultAgentPath()
	if filepath.Base(path) != "agent.config.json" {
		t.Errorf("DefaultAgentPath() = %q, want basename %q", path, "agent.config.json")
	}
}

func TestDefaultAdminPathEndsWithAdminConfigJSON(t *testing.T) {
	path := DefaultAdminPath()
	if filepath.Base(path) != "admin.config.json" {
		t.Errorf("DefaultAdminPath() = %q, want basename %q", path, "admin.config.json")
	}
}
