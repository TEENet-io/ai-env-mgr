package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const SettingDataRetention = "data_retention"

type DataRetentionSettings struct {
	Enabled bool `json:"enabled"`
	Days    int  `json:"days"`
}

func DefaultDataRetentionSettings() DataRetentionSettings {
	return DataRetentionSettings{Enabled: false, Days: 30}
}

func (s DataRetentionSettings) Validate() error {
	if s.Days < 1 || s.Days > 3650 {
		return errors.New("data retention must be between 1 and 3650 days")
	}
	return nil
}

func LoadDataRetentionSettings(ctx context.Context, settings Settings) (DataRetentionSettings, int, error) {
	v, err := settings.Get(ctx, SettingDataRetention)
	if errors.Is(err, ErrNotFound) {
		return DefaultDataRetentionSettings(), 0, nil
	}
	if err != nil {
		return DataRetentionSettings{}, 0, err
	}
	var out DataRetentionSettings
	if err := json.Unmarshal(v.Value, &out); err != nil {
		return DataRetentionSettings{}, 0, fmt.Errorf("the stored data retention settings are not readable: %w", err)
	}
	if out.Days == 0 {
		out.Days = DefaultDataRetentionSettings().Days
	}
	return out, v.Version, nil
}
