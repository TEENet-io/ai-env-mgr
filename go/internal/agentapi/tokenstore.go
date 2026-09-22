package agentapi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// tokenFile is where the device token lives, under the agent's state
// directory. On Windows its contents are DPAPI-sealed to the machine; the
// file is readable by the service and by nobody else worth mentioning.
const tokenFile = "device.token"

// ErrNoToken means the machine has not enrolled yet.
var ErrNoToken = errors.New("no device token on this machine")

// LoadToken reads the token saved by SaveToken.
func LoadToken(stateDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, tokenFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoToken
	}
	if err != nil {
		return "", err
	}
	plain, err := unprotect(raw)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(plain))
	if token == "" {
		return "", ErrNoToken
	}
	return token, nil
}

// SaveToken writes the token, sealed where the platform can seal it, and
// replaces any earlier one atomically.
func SaveToken(stateDir, token string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	sealed, err := protect([]byte(token))
	if err != nil {
		return err
	}
	tmp := filepath.Join(stateDir, tokenFile+".tmp")
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, tokenFile))
}

// ForgetToken removes the token: the console refused it and the machine
// will enrol again.
func ForgetToken(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, tokenFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
