//go:build !windows

package policy

import (
	"errors"

	"github.com/TEENet-io/airlock/internal/model"
)

// Apply is a non-Windows stand-in for the real HKLM-writing implementation
// in registry_windows.go. The browser policies this package manages only
// exist as Windows registry keys, so there is nothing to apply on any other
// OS. This stub exists so the package (and anything that imports it) still
// builds, vets and unit-tests on Linux/macOS dev machines and CI, while the
// agent binary that actually calls Apply is always built for windows.
func Apply(p model.Policy) error {
	return errors.New("policy.Apply: registry-backed browser blocking is only supported on Windows")
}

// Current is the non-Windows stand-in for reading the applied policy back out
// of the registry. There is no registry here, so it reports nothing applied.
func Current() (model.Policy, error) {
	return model.Policy{}, nil
}
