package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// SettingGatewayChannels records which suppliers (litellm.ChannelOf) an
// administrator has paused. A paused channel's models leave the employees'
// pickers, and employees allowed "every model" are held to the other
// channels' models on the gateway until it is resumed.
const SettingGatewayChannels = "gateway_channels"

// GatewayChannels is the stored value.
type GatewayChannels struct {
	Paused map[string]ChannelPause `json:"paused"`
}

// ChannelPause is who paused a channel, when, and why.
type ChannelPause struct {
	At     string `json:"at"`
	By     string `json:"by"`
	Reason string `json:"reason,omitempty"`
}

// PausedSet is the paused channel ids, for litellm.WithoutChannels.
func (g GatewayChannels) PausedSet() map[string]bool {
	out := map[string]bool{}
	for id := range g.Paused {
		out[id] = true
	}
	return out
}

// LoadGatewayChannels reads the setting; nothing stored is "nothing paused"
// at version 0.
func LoadGatewayChannels(ctx context.Context, settings Settings) (GatewayChannels, int, error) {
	g := GatewayChannels{Paused: map[string]ChannelPause{}}
	setting, err := settings.Get(ctx, SettingGatewayChannels)
	if errors.Is(err, ErrNotFound) {
		return g, 0, nil
	}
	if err != nil {
		return g, 0, err
	}
	if err := json.Unmarshal(setting.Value, &g); err != nil {
		return g, 0, fmt.Errorf("the stored gateway channels are not readable: %w", err)
	}
	if g.Paused == nil {
		g.Paused = map[string]ChannelPause{}
	}
	return g, setting.Version, nil
}
