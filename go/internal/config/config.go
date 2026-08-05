// Package config loads the shared OSS connection settings used by both
// agent.exe and admin.exe. The two binaries read the same JSON shape but
// from different files with different key permissions.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultIntervalMinutes is used whenever intervalMinutes is missing or
// non-positive in the config file.
const DefaultIntervalMinutes = 30

// Config holds the OSS credentials and sync cadence shared by agent and
// admin. It is loaded from a JSON file next to the executable.
type Config struct {
	Bucket          string `json:"bucket"`
	Endpoint        string `json:"endpoint"`
	AccessKeyID     string `json:"accessKeyId"`
	AccessKeySecret string `json:"accessKeySecret"`
	IntervalMinutes int    `json:"intervalMinutes"`
}

// Load reads and parses the config file at path, validating that all
// required fields are present. IntervalMinutes falls back to
// DefaultIntervalMinutes when zero or negative.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.finalise(fmt.Sprintf("config %s", path)); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// finalise checks the required fields and applies defaults. origin names the
// place the settings came from so the error says which one to go fix -- the
// agent can be configured from a file or from values baked into the binary.
func (c *Config) finalise(origin string) error {
	for _, f := range []struct {
		name  string
		value string
	}{
		{"bucket", c.Bucket},
		{"endpoint", c.Endpoint},
		{"accessKeyId", c.AccessKeyID},
		{"accessKeySecret", c.AccessKeySecret},
	} {
		if f.value == "" {
			return fmt.Errorf("%s: missing required field %q", origin, f.name)
		}
	}
	if c.IntervalMinutes <= 0 {
		c.IntervalMinutes = DefaultIntervalMinutes
	}
	return nil
}

// Where a binary's settings came from. Reported by `agent.exe status` so a
// support call does not have to guess which one is in play.
const (
	SourceBuiltIn = "built-in"
	SourceFile    = "config file"
)

// Resolve picks between the credentials written into the binary's source and
// a configuration file next to the executable.
//
// Built-in wins. A shipped agent.exe must not be redirected at someone else's
// bucket by dropping a config file beside it, so the file is only consulted
// when the binary carries no credentials -- which is how developer builds and
// the test rig run.
//
// Each command keeps its own credentials in its own package, so agent.exe
// never contains the administrator's read-write key and vice versa.
func Resolve(builtIn Config, path string) (*Config, string, error) {
	if builtIn.AccessKeyID != "" {
		if err := builtIn.finalise("built-in credentials"); err != nil {
			return nil, "", err
		}
		return &builtIn, SourceBuiltIn, nil
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, SourceFile, nil
}

// HasBuiltIn reports whether a binary carries its own credentials.
func HasBuiltIn(builtIn Config) bool { return builtIn.AccessKeyID != "" }

// Source names where a binary would get its settings, without loading them.
func Source(builtIn Config) string {
	if HasBuiltIn(builtIn) {
		return SourceBuiltIn
	}
	return SourceFile
}

// DefaultAgentPath returns the path to agent.config.json next to the
// currently running executable. If the executable path cannot be
// determined, it falls back to the bare filename.
func DefaultAgentPath() string {
	return defaultPath("agent.config.json")
}

// DefaultAdminPath returns the path to admin.config.json next to the
// currently running executable. If the executable path cannot be
// determined, it falls back to the bare filename.
func DefaultAdminPath() string {
	return defaultPath("admin.config.json")
}

// defaultPath resolves name relative to the running executable's directory,
// falling back to the bare name when the executable path is unavailable.
func defaultPath(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	return filepath.Join(filepath.Dir(exe), name)
}
