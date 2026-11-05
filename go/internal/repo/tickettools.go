package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// SettingTicketTools holds TicketToolSettings as JSON.
const SettingTicketTools = "ticket_tools"

// TicketToolSettings lists the custom tools a ticket can name, by the names
// customers know them by. It is one list for the whole console: nothing here
// records which person may use which tool.
type TicketToolSettings struct {
	Names []string `json:"names"`
}

func LoadTicketToolSettings(ctx context.Context, settings Settings) (TicketToolSettings, int, error) {
	v, err := settings.Get(ctx, SettingTicketTools)
	if errors.Is(err, ErrNotFound) {
		return TicketToolSettings{}, 0, nil
	}
	if err != nil {
		return TicketToolSettings{}, 0, err
	}
	var out TicketToolSettings
	if err := json.Unmarshal(v.Value, &out); err != nil {
		return TicketToolSettings{}, 0, fmt.Errorf("the stored ticket tools are not readable: %w", err)
	}
	return out, v.Version, nil
}
