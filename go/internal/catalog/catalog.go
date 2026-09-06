// Package catalog builds the Codex model catalog (models.json) that decides
// what appears in the model picker.
//
// The catalog is a full replacement, not a patch, and Codex loads it once at
// app-server startup -- so a delivered file only takes effect after every
// Codex process has exited and restarted.
//
// Nothing here maintains a list of models. The line-up and its metadata come
// from the gateway, so adding a model there is enough to make it show up for
// employees on their next sync.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
)

// templateJSON holds one complete catalog entry taken from a codex-kiosk
// release, plus the client version it came from.
//
// A catalog entry has around 35 fields, most of which are long prompts and
// tool declarations that only OpenAI publishes. Synthesizing them would mean
// guessing at Codex's internals, so the entry is carried verbatim and only
// the handful of fields that identify a model are substituted. Refresh this
// file when Codex changes major version.
//
//go:embed template.json
var templateJSON []byte

// customProviderPatch is the set of fields that must be neutralized for any
// model reached through a gateway rather than Codex's own backend.
//
// Left at their upstream values, Codex builds requests in the Responses Lite
// and multi-agent collaboration shapes that only its own backend answers;
// a gateway receives something it cannot serve. This mirrors the same
// override applied by codex-only-local's build-api-model-catalog.mjs.
var customProviderPatch = map[string]any{
	"tool_mode":           nil,
	"multi_agent_version": nil,
	"use_responses_lite":  false,
}

// upstreamIdentity is the opening sentence of the instructions the template
// carries. It is OpenAI's own text for their own model; copied verbatim to a
// model that is not GPT-5, it makes that model introduce itself as GPT-5 when
// asked -- which reads, to an employee who just picked Grok in the picker, as
// the gateway having routed them somewhere else.
//
// Only this sentence is rewritten. The 17,000 characters after it describe
// Codex's tool protocol and are what make apply_patch and the shell work at
// all; "Codex" is the product persona and stays.
const upstreamIdentity = "You are Codex, an agent based on GPT-5."

// reasoningDescriptions are the picker's sub-labels for each effort level.
var reasoningDescriptions = map[string]string{
	"low":    "Fast responses with lighter reasoning",
	"medium": "Balances speed and reasoning depth for everyday tasks",
	"high":   "Greater reasoning depth for complex problems",
	"xhigh":  "Extra high reasoning depth for complex problems",
	"max":    "Maximum reasoning depth for the hardest problems",
}

type embedded struct {
	ClientVersion string         `json:"client_version"`
	Template      map[string]any `json:"template"`
}

// Build renders a catalog for one employee.
//
// allowed is that employee's model allowlist as the gateway holds it. Models
// outside it are omitted rather than shown-and-refused: an entry the picker
// offers but the gateway rejects reads as a broken product, which is worse
// than never listing it. A nil allowlist means the gateway placed no
// restriction, so everything visible is included.
//
// Building with no models left is an error. An empty catalog empties the
// picker, and shipping that on the back of a transient gateway hiccup would
// take away every model the employee had.
func Build(models []litellm.Model, allowed []string) ([]byte, error) {
	var tpl embedded
	if err := json.Unmarshal(templateJSON, &tpl); err != nil {
		return nil, fmt.Errorf("decode embedded catalog template: %w", err)
	}
	if len(tpl.Template) == 0 {
		return nil, fmt.Errorf("embedded catalog template is empty")
	}

	permitted := map[string]bool{}
	for _, m := range allowed {
		permitted[m] = true
	}

	entries := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if len(permitted) > 0 && !permitted[m.Name] {
			continue
		}
		entries = append(entries, entryFor(tpl.Template, m))
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("catalog would be empty: %d gateway models, %d allowed", len(models), len(allowed))
	}

	// Stable order so an unchanged line-up produces an unchanged file, and
	// redelivering does not look like a change to anyone diffing it.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i]["slug"].(string) < entries[j]["slug"].(string)
	})
	for i, e := range entries {
		e["priority"] = i + 1
	}

	out, err := json.MarshalIndent(map[string]any{
		"client_version": tpl.ClientVersion,
		"etag":           nil,
		"models":         entries,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode catalog: %w", err)
	}
	return append(out, '\n'), nil
}

// entryFor clones the template and overlays one model's identity.
func entryFor(template map[string]any, m litellm.Model) map[string]any {
	entry := make(map[string]any, len(template))
	for k, v := range template {
		entry[k] = v
	}
	for k, v := range customProviderPatch {
		entry[k] = v
	}
	rewriteIdentity(entry, m)

	// slug is the routing key: the picker sends it back as the `model` field
	// and the gateway routes on it, so it must equal the gateway's
	// model_name character for character.
	entry["slug"] = m.Name
	entry["visibility"] = "list"
	entry["supported_in_api"] = true

	if m.Info.DisplayName != "" {
		entry["display_name"] = m.Info.DisplayName
	} else {
		// Better the raw slug than the template's leftover name, which
		// would label every model as whichever one the template came from.
		entry["display_name"] = m.Name
	}
	if m.Info.ContextWindow > 0 {
		entry["context_window"] = m.Info.ContextWindow
		entry["max_context_window"] = m.Info.ContextWindow
	}
	if len(m.Info.Modalities) > 0 {
		entry["input_modalities"] = m.Info.Modalities
		entry["web_search_tool_type"] = "text"
		for _, mod := range m.Info.Modalities {
			if mod == "image" {
				entry["web_search_tool_type"] = "text_and_image"
			}
		}
	}
	// apply_patch_tool_type is deliberately never changed from the template.
	// Codex 0.148's ApplyPatchToolType enum has exactly one variant,
	// "freeform"; any other string makes serde reject the whole catalog,
	// Codex silently falls back to its default provider, and the employee is
	// shown a ChatGPT sign-in. That happened on 2026-09-07 with "function".
	// Upstreams that refuse custom tools are handled at the gateway instead.
	if levels := reasoningLevels(m.Info.ReasoningLevels); len(levels) > 0 {
		entry["supported_reasoning_levels"] = levels
		entry["default_reasoning_level"] = defaultReasoning(m.Info, levels)
	}
	return entry
}

// rewriteIdentity replaces the template's "based on GPT-5" introduction with
// the model's real name. The nested map is copied first: entryFor's clone is
// shallow, and editing model_messages in place would rewrite it for every
// model built from the same template -- the last one would win.
//
// A template whose opening sentence is not the known one is left untouched.
// Guessing at a different sentence would risk damaging the protocol text.
func rewriteIdentity(entry map[string]any, m litellm.Model) {
	messages, ok := entry["model_messages"].(map[string]any)
	if !ok {
		return
	}
	text, ok := messages["instructions_template"].(string)
	if !ok || !strings.HasPrefix(text, upstreamIdentity) {
		return
	}
	name := m.Info.DisplayName
	if name == "" {
		name = m.Name
	}
	replacement := fmt.Sprintf("You are Codex, a coding agent running on %s.", name)

	copied := make(map[string]any, len(messages))
	for k, v := range messages {
		copied[k] = v
	}
	copied["instructions_template"] = replacement + strings.TrimPrefix(text, upstreamIdentity)
	entry["model_messages"] = copied
}

func reasoningLevels(levels []string) []map[string]string {
	out := make([]map[string]string, 0, len(levels))
	for _, l := range levels {
		desc, ok := reasoningDescriptions[l]
		if !ok {
			// An effort level Codex does not know would be offered in the
			// picker and rejected on use, so skip it rather than guess.
			continue
		}
		out = append(out, map[string]string{"effort": l, "description": desc})
	}
	return out
}

// defaultReasoning picks the level the picker starts on, falling back to the
// first supported level when the declared default is not among them.
func defaultReasoning(info litellm.ModelInfo, levels []map[string]string) string {
	for _, l := range levels {
		if l["effort"] == info.DefaultReasoning {
			return info.DefaultReasoning
		}
	}
	return levels[0]["effort"]
}
