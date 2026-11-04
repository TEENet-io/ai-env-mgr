package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// SettingDeviceChannel holds DeviceChannelSettings as JSON.
const SettingDeviceChannel = "device_channel"

// DeviceChannelSettings govern the agents' channel to the console. (The
// stored value may still carry write_oss_objects and import_oss_status from
// when agents also read the bucket; they are ignored.)
type DeviceChannelSettings struct {
	// EnrolCIDRs, when set, is the only place enrolments are accepted from.
	EnrolCIDRs []string `json:"enrol_cidrs"`
}

func DefaultDeviceChannelSettings() DeviceChannelSettings {
	return DeviceChannelSettings{}
}

// ParsedCIDRs returns the enrolment networks, rejecting anything that is
// not a network.
func (s DeviceChannelSettings) ParsedCIDRs() ([]net.IPNet, error) {
	out := make([]net.IPNet, 0, len(s.EnrolCIDRs))
	for _, text := range s.EnrolCIDRs {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if !strings.Contains(text, "/") {
			if net.ParseIP(text) == nil {
				return nil, fmt.Errorf("%q is not an address or a network", text)
			}
			if strings.Contains(text, ":") {
				text += "/128"
			} else {
				text += "/32"
			}
		}
		_, n, err := net.ParseCIDR(text)
		if err != nil {
			return nil, fmt.Errorf("%q is not a network: %w", text, err)
		}
		out = append(out, *n)
	}
	return out, nil
}

func (s DeviceChannelSettings) Validate() error {
	_, err := s.ParsedCIDRs()
	return err
}

func LoadDeviceChannelSettings(ctx context.Context, settings Settings) (DeviceChannelSettings, int, error) {
	setting, err := settings.Get(ctx, SettingDeviceChannel)
	if errors.Is(err, ErrNotFound) {
		return DefaultDeviceChannelSettings(), 0, nil
	}
	if err != nil {
		return DeviceChannelSettings{}, 0, err
	}
	var s DeviceChannelSettings
	if err := json.Unmarshal(setting.Value, &s); err != nil {
		return DeviceChannelSettings{}, 0, fmt.Errorf("the stored device channel settings are not readable: %w", err)
	}
	return s, setting.Version, nil
}
