package managedskills

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

const RefreshManifestName = "company-skills.json"

// RefreshManifest contains no skill bodies, keys, commands, or user paths.
// The protected machine directory makes it readable by desktop sessions and
// writable only by the administrator Agent.
type RefreshManifest struct {
	Version    int                         `json:"version"`
	Generation string                      `json:"generation"`
	Users      []string                    `json:"users"`
	Skills     []model.ManagedSkillVersion `json:"skills"`
}

// DesktopRefreshed reads an unprivileged, informational acknowledgement. It
// never executes a command or grants access based on desktop-controlled data.
func DesktopRefreshed(profile, user, generation string) (bool, error) {
	if profile == "" || user == "" || strings.ContainsAny(user, `/\:`) || user == "." || user == ".." {
		return false, errors.New("invalid desktop refresh user")
	}
	path := filepath.Join(profile, "AppData", "Local", "TEENet", "company-skills-refresh.json")
	if err := safePath(path); err != nil {
		return false, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return false, errors.New("invalid desktop refresh acknowledgement")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return false, err
	}
	if len(data) > 4096 {
		return false, errors.New("desktop refresh acknowledgement too large")
	}
	var ack struct {
		Version    int    `json:"version"`
		Generation string `json:"generation"`
		User       string `json:"user"`
	}
	if err := json.Unmarshal(data, &ack); err != nil {
		return false, errors.New("invalid desktop refresh acknowledgement JSON")
	}
	return ack.Version == 1 && ack.Generation == generation && strings.EqualFold(ack.User, user), nil
}
