package creds

import (
	"fmt"
	"strings"
)

// RenderGatewayConfig produces the config.toml fragment that points Codex at
// the gateway.
//
// Only the keys this tool owns appear here. The agent merges the fragment into
// whatever the employee already has rather than replacing the file, which is
// what lets somebody keep their own settings.
//
// It lives in this package because two callers need it -- the console that
// publishes credentials today and the Worker that exports them from the
// database -- and two renderings of one file is how a machine ends up pointed
// at a base URL nobody meant.
func RenderGatewayConfig(baseURL, windowsUser string, allowed []string, token string) string {
	model := ""
	if len(allowed) > 0 {
		model = allowed[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "model              = %q\n", model)
	fmt.Fprintf(&b, "model_provider     = \"gateway\"\n")
	// Windows paths are written with forward slashes in TOML.
	fmt.Fprintf(&b, "model_catalog_json = \"C:/Users/%s/.codex/models.json\"\n", windowsUser)
	fmt.Fprintf(&b, "web_search         = \"live\"\n")
	// Codex agent runs go for many minutes without emitting a token; the
	// default idle timeout would cut them off mid-task.
	fmt.Fprintf(&b, "stream_idle_timeout_ms = 7200000\n\n")
	fmt.Fprintf(&b, "[model_providers.gateway]\n")
	fmt.Fprintf(&b, "name     = \"Gateway\"\n")
	fmt.Fprintf(&b, "base_url = %q\n", strings.TrimRight(baseURL, "/")+"/v1")
	fmt.Fprintf(&b, "wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "experimental_bearer_token = %q\n", token)
	return b.String()
}
