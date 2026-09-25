package litellm

import (
	"net/url"
	"sort"
	"strings"
)

// A channel is the supplier a model is bought from: the unit an outage, a
// billing problem or a price change arrives in, and so the unit an
// administrator switches off.
const (
	ChannelAWS    = "aws"
	ChannelGoogle = "google"
	ChannelAzure  = "azure"
	ChannelOpenAI = "openai"
	ChannelOther  = "other"
)

// ChannelLabel is how the console names a channel.
func ChannelLabel(id string) string {
	switch id {
	case ChannelAWS:
		return "AWS Bedrock"
	case ChannelGoogle:
		return "Google Vertex"
	case ChannelAzure:
		return "Azure OpenAI"
	case ChannelOpenAI:
		return "OpenAI"
	}
	return "未标注"
}

// ChannelOf says which supplier serves a deployment. An explicit
// model_info.channel wins. Otherwise LiteLLM's provider decides, except
// that the OpenAI-compatible route pointed at an *.azure.com endpoint is
// Azure: that is how the gateway reaches its Azure deployments, and it
// reports them as "openai".
func ChannelOf(m Model) string {
	if c := strings.ToLower(strings.TrimSpace(m.Info.Channel)); c != "" {
		return c
	}
	provider := strings.ToLower(m.Info.LitellmProvider)
	if provider == "" {
		if i := strings.Index(m.Params.Model, "/"); i > 0 {
			provider = strings.ToLower(m.Params.Model[:i])
		}
	}
	switch {
	case provider == "bedrock" || strings.HasPrefix(provider, "bedrock"):
		return ChannelAWS
	case strings.HasPrefix(provider, "vertex_ai"), provider == "gemini":
		return ChannelGoogle
	case strings.HasPrefix(provider, "azure"):
		return ChannelAzure
	case provider == "openai":
		if u, err := url.Parse(m.Params.APIBase); err == nil && strings.HasSuffix(strings.ToLower(u.Hostname()), ".azure.com") {
			return ChannelAzure
		}
		return ChannelOpenAI
	}
	return ChannelOther
}

// WithoutChannels drops the deployments of paused channels. A model name
// that another, unpaused channel still serves stays.
func WithoutChannels(models []Model, paused map[string]bool) []Model {
	if len(paused) == 0 {
		return models
	}
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if !paused[ChannelOf(m)] {
			out = append(out, m)
		}
	}
	return out
}

// Names lists the distinct model names, sorted.
func Names(models []Model) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range models {
		if !seen[m.Name] {
			seen[m.Name] = true
			out = append(out, m.Name)
		}
	}
	sort.Strings(out)
	return out
}
