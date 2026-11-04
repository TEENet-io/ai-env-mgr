package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// SettingGatewayTeam holds GatewayTeamSettings as JSON.
const SettingGatewayTeam = "gateway_team"

// GatewayTeamSettings name this customer's LiteLLM team on a shared gateway.
// Empty: the gateway is not shared and nothing is named under a team.
type GatewayTeamSettings struct {
	TeamID string `json:"team_id"`
}

func LoadGatewayTeamSettings(ctx context.Context, settings Settings) (GatewayTeamSettings, int, error) {
	v, err := settings.Get(ctx, SettingGatewayTeam)
	if errors.Is(err, ErrNotFound) {
		return GatewayTeamSettings{}, 0, nil
	}
	if err != nil {
		return GatewayTeamSettings{}, 0, err
	}
	var out GatewayTeamSettings
	if err := json.Unmarshal(v.Value, &out); err != nil {
		return GatewayTeamSettings{}, 0, fmt.Errorf("the stored gateway team is not readable: %w", err)
	}
	return out, v.Version, nil
}
