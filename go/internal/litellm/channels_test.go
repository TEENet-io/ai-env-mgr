package litellm

import "testing"

// The shapes are the live gateway's /model/info (2026-09-25).
func TestChannelOfTheGatewaysDeployments(t *testing.T) {
	cases := []struct {
		m    Model
		want string
	}{
		{Model{Name: "grok-4.6", Info: ModelInfo{LitellmProvider: "bedrock"}, Params: ModelParams{Model: "bedrock/converse/global.xai.grok-4.6"}}, ChannelAWS},
		{Model{Name: "gemini-3.1-pro", Info: ModelInfo{LitellmProvider: "vertex_ai"}, Params: ModelParams{Model: "vertex_ai/gemini-3.1-pro-preview"}}, ChannelGoogle},
		{Model{Name: "qwen3-coder-480b", Info: ModelInfo{LitellmProvider: "vertex_ai-qwen_models"}}, ChannelGoogle},
		{Model{Name: "gemini-embedding-001", Info: ModelInfo{LitellmProvider: "vertex_ai-embedding-models"}}, ChannelGoogle},
		{Model{Name: "gpt-6-astra", Info: ModelInfo{LitellmProvider: "openai"}, Params: ModelParams{Model: "openai/gpt-6-astra-1", APIBase: "https://user05-2635-resource.services.ai.azure.com/openai/v1"}}, ChannelAzure},
		{Model{Name: "gpt-x", Info: ModelInfo{LitellmProvider: "openai"}, Params: ModelParams{APIBase: "https://api.openai.com/v1"}}, ChannelOpenAI},
		{Model{Name: "tagged", Info: ModelInfo{LitellmProvider: "openai", Channel: "Azure"}}, ChannelAzure},
		{Model{Name: "mystery"}, ChannelOther},
	}
	for _, c := range cases {
		if got := ChannelOf(c.m); got != c.want {
			t.Errorf("%s: %s, want %s", c.m.Name, got, c.want)
		}
	}
	models := []Model{cases[0].m, cases[1].m, cases[4].m,
		{Name: "qwen3-coder-480b", Info: ModelInfo{LitellmProvider: "bedrock"}},
		{Name: "qwen3-coder-480b", Info: ModelInfo{LitellmProvider: "vertex_ai-qwen_models"}}}
	left := Names(WithoutChannels(models, map[string]bool{ChannelGoogle: true}))
	if len(left) != 3 || left[0] != "gpt-6-astra" || left[1] != "grok-4.6" || left[2] != "qwen3-coder-480b" {
		t.Fatalf("pausing Google leaves %v; a model another channel serves must stay", left)
	}
}
