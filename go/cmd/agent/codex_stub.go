//go:build !windows

package main

import "github.com/TEENet-io/ai-env-mgr/internal/agentcore"

// newCodexInstaller returns nothing off Windows: the Codex desktop only exists
// there, and the sync logic treats a nil installer as "this machine does not
// do Codex updates". Keeping the stub lets the whole agent build and its tests
// run on the machine it is developed from.
func newCodexInstaller() agentcore.CodexInstaller { return nil }
