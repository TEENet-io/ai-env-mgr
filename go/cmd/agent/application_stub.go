//go:build !windows

package main

import "github.com/TEENet-io/ai-env-mgr/internal/agentcore"

// Applications are Windows-only. Keeping the nil implementation lets Linux
// CI exercise the rest of the sync path without pretending to install apps.
func newApplicationInstaller() agentcore.ApplicationInstaller { return nil }
