package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
)

func gatewayModels() []litellm.Model {
	return []litellm.Model{
		{Name: "grok-4.6", Info: litellm.ModelInfo{
			DisplayName: "Grok 4.6", ContextWindow: 256000,
			ReasoningLevels: []string{"low", "high"}, DefaultReasoning: "high",
			Modalities: []string{"text", "image"}, CatalogVisible: true,
		}},
		{Name: "glm-5", Info: litellm.ModelInfo{
			DisplayName: "智谱 GLM-5", ContextWindow: 128000,
			ReasoningLevels: []string{"low", "high"}, DefaultReasoning: "high",
			Modalities: []string{"text"}, CatalogVisible: true,
		}},
	}
}

func decode(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("catalog is not valid JSON: %v", err)
	}
	return doc.Models
}

func find(entries []map[string]any, slug string) map[string]any {
	for _, e := range entries {
		if e["slug"] == slug {
			return e
		}
	}
	return nil
}

func TestBuildAppliesCustomProviderPatch(t *testing.T) {
	// Without these three, Codex talks to the gateway in shapes only its own
	// backend answers. This is the single most important property here.
	raw, err := Build(gatewayModels(), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, e := range decode(t, raw) {
		if e["tool_mode"] != nil {
			t.Errorf("%v: tool_mode = %v, want null", e["slug"], e["tool_mode"])
		}
		if e["multi_agent_version"] != nil {
			t.Errorf("%v: multi_agent_version = %v, want null", e["slug"], e["multi_agent_version"])
		}
		if e["use_responses_lite"] != false {
			t.Errorf("%v: use_responses_lite = %v, want false", e["slug"], e["use_responses_lite"])
		}
	}
}

func TestBuildCarriesGatewayMetadata(t *testing.T) {
	raw, err := Build(gatewayModels(), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	entries := decode(t, raw)

	grok := find(entries, "grok-4.6")
	if grok == nil {
		t.Fatal("grok-4.6 missing from catalog")
	}
	if grok["display_name"] != "Grok 4.6" {
		t.Errorf("display_name = %v", grok["display_name"])
	}
	if grok["context_window"] != float64(256000) || grok["max_context_window"] != float64(256000) {
		t.Errorf("context window not applied: %v / %v", grok["context_window"], grok["max_context_window"])
	}
	if grok["web_search_tool_type"] != "text_and_image" {
		t.Errorf("image modality should widen web_search_tool_type, got %v", grok["web_search_tool_type"])
	}
	if grok["default_reasoning_level"] != "high" {
		t.Errorf("default_reasoning_level = %v", grok["default_reasoning_level"])
	}

	glm := find(entries, "glm-5")
	if glm["web_search_tool_type"] != "text" {
		t.Errorf("text-only model should stay text, got %v", glm["web_search_tool_type"])
	}
}

func TestBuildRestrictsToAllowlist(t *testing.T) {
	// An entry the picker offers but the gateway refuses reads as broken.
	raw, err := Build(gatewayModels(), []string{"glm-5"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	entries := decode(t, raw)
	if len(entries) != 1 || entries[0]["slug"] != "glm-5" {
		t.Fatalf("allowlist not honored: %v", entries)
	}
}

func TestBuildRefusesEmptyCatalog(t *testing.T) {
	// Delivering an empty catalog would strip every model from the picker.
	if _, err := Build(gatewayModels(), []string{"model-that-is-gone"}); err == nil {
		t.Fatal("expected an error rather than an empty catalog")
	}
	if _, err := Build(nil, nil); err == nil {
		t.Fatal("expected an error when the gateway returns no models")
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	// Redelivering an unchanged line-up must produce an identical file.
	a, err := Build(gatewayModels(), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	reversed := []litellm.Model{gatewayModels()[1], gatewayModels()[0]}
	b, err := Build(reversed, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if string(a) != string(b) {
		t.Error("catalog depends on the order the gateway listed models")
	}
}

func TestBuildFallsBackToSlugWhenNoDisplayName(t *testing.T) {
	// Otherwise every model inherits the template's name.
	raw, err := Build([]litellm.Model{{Name: "bare-model", Info: litellm.ModelInfo{CatalogVisible: true}}}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	entries := decode(t, raw)
	if entries[0]["display_name"] != "bare-model" {
		t.Errorf("display_name = %v, want the slug", entries[0]["display_name"])
	}
}

func TestBuildDropsUnknownReasoningLevels(t *testing.T) {
	raw, err := Build([]litellm.Model{{Name: "m", Info: litellm.ModelInfo{
		CatalogVisible: true, ReasoningLevels: []string{"low", "turbo"}, DefaultReasoning: "turbo",
	}}}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	entries := decode(t, raw)
	levels, _ := entries[0]["supported_reasoning_levels"].([]any)
	if len(levels) != 1 {
		t.Fatalf("unknown effort level was kept: %v", levels)
	}
	if entries[0]["default_reasoning_level"] != "low" {
		t.Errorf("default should fall back to a supported level, got %v", entries[0]["default_reasoning_level"])
	}
}

func instructionsOf(t *testing.T, e map[string]any) string {
	t.Helper()
	msgs, _ := e["model_messages"].(map[string]any)
	text, _ := msgs["instructions_template"].(string)
	if text == "" {
		t.Fatalf("entry %v has no instructions_template", e["slug"])
	}
	return text
}

func TestBuildNamesTheRealModelInItsInstructions(t *testing.T) {
	// The template's first sentence says "based on GPT-5". Sent to Grok, Grok
	// says it is GPT-5 -- and the employee concludes the gateway lied.
	raw, err := Build(gatewayModels(), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	entries := decode(t, raw)

	grok := instructionsOf(t, find(entries, "grok-4.6"))
	if !strings.HasPrefix(grok, "You are Codex, a coding agent running on Grok 4.6.") {
		t.Errorf("grok's instructions still claim another model:\n%.120s", grok)
	}
	if strings.Contains(grok, "based on GPT-5") {
		t.Errorf("GPT-5 identity survived rewrite")
	}
	// Everything after the first sentence -- the tool protocol -- is intact.
	if !strings.Contains(grok, "You and the user share one workspace") {
		t.Errorf("protocol text after the identity sentence was damaged")
	}

	// Each entry gets its own sentence; a shared nested map would make every
	// model introduce itself as whichever one was built last.
	glm := instructionsOf(t, find(entries, "glm-5"))
	if !strings.HasPrefix(glm, "You are Codex, a coding agent running on 智谱 GLM-5.") {
		t.Errorf("glm's instructions carry the wrong name:\n%.120s", glm)
	}
}

func TestIdentityRewriteFallsBackToSlug(t *testing.T) {
	raw, err := Build([]litellm.Model{{Name: "bare-model", Info: litellm.ModelInfo{CatalogVisible: true}}}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	text := instructionsOf(t, decode(t, raw)[0])
	if !strings.HasPrefix(text, "You are Codex, a coding agent running on bare-model.") {
		t.Errorf("slug fallback missing:\n%.120s", text)
	}
}

func TestApplyPatchToolTypeFollowsGatewayFlag(t *testing.T) {
	raw, err := Build([]litellm.Model{
		{Name: "compat", Info: litellm.ModelInfo{CatalogVisible: true, ApplyPatchTool: "function"}},
		{Name: "native", Info: litellm.ModelInfo{CatalogVisible: true}},
		{Name: "odd", Info: litellm.ModelInfo{CatalogVisible: true, ApplyPatchTool: "grammar"}},
	}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	entries := decode(t, raw)
	if got := find(entries, "compat")["apply_patch_tool_type"]; got != "function" {
		t.Errorf("compat model should declare apply_patch as a function tool, got %v", got)
	}
	// A function-only upstream refuses the custom form; a native one wants it.
	if got := find(entries, "native")["apply_patch_tool_type"]; got != "freeform" {
		t.Errorf("native model should keep the template's freeform, got %v", got)
	}
	if got := find(entries, "odd")["apply_patch_tool_type"]; got != "freeform" {
		t.Errorf("an unknown value must not be written into the catalog, got %v", got)
	}
}
