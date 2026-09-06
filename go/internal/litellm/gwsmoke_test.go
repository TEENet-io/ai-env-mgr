package litellm

import (
	"context"
	"os"
	"testing"
)

// TestAgainstRealGateway checks that a live gateway declares the catalog
// metadata this tool depends on.
//
// It is opt-in: without GW_URL and GW_ADMIN_KEY it skips, so `go test ./...`
// stays hermetic. Run it after changing the gateway's model list -- a model
// added without model_info would otherwise reach employees as an entry the
// picker labels with its raw slug and sizes with the template's context
// window.
func TestAgainstRealGateway(t *testing.T) {
	base, key := os.Getenv("GW_URL"), os.Getenv("GW_ADMIN_KEY")
	if base == "" || key == "" {
		t.Skip("set GW_URL and GW_ADMIN_KEY to run against a live gateway")
	}

	models, err := New(base, key).Models(context.Background())
	if err != nil {
		t.Fatalf("read model catalog: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("gateway reports no catalog-visible models")
	}
	for _, m := range models {
		t.Logf("%-14s %-14s ctx=%d reasoning=%v", m.Name, m.Info.DisplayName, m.Info.ContextWindow, m.Info.ReasoningLevels)
		if m.Info.DisplayName == "" {
			t.Errorf("%s has no display_name; the picker would show the raw slug", m.Name)
		}
		if m.Info.ContextWindow == 0 {
			t.Errorf("%s has no context_window; Codex would use the template's", m.Name)
		}
	}
}
