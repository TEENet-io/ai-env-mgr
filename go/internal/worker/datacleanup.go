package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type DataCleanupObjects interface {
	ListInfo(prefix string) ([]ossclient.ObjectInfo, error)
	Delete(key string) error
}

// DataCleanup enforces the retention value in AlertSettings. It only deletes
// objects identified by DataCollectUser, so operational state and installers
// cannot be swept by a retention task.
type DataCleanup struct {
	Store   repo.Store
	Objects DataCleanupObjects
	Now     func() time.Time
}

func (h DataCleanup) Run(ctx context.Context, _ repo.Task) (Result, error) {
	if h.Now == nil {
		h.Now = func() time.Time { return time.Now().UTC() }
	}
	settings, _, err := repo.LoadDataRetentionSettings(ctx, h.Store.Settings())
	if err != nil {
		return Result{}, err
	}
	if !settings.Enabled {
		return Result{Note: "automatic data cleanup is disabled"}, nil
	}
	cutoff := h.Now().UTC().Add(-time.Duration(settings.Days) * 24 * time.Hour)
	infos, err := h.Objects.ListInfo(ossclient.Root)
	if err != nil {
		return Result{}, ClassError("oss_list", err)
	}
	removed := 0
	for _, info := range infos {
		if _, _, ok := ossclient.DataCollectUser(info.Key); !ok || !info.LastModified.Before(cutoff) {
			continue
		}
		if err := h.Objects.Delete(info.Key); err != nil {
			return Result{}, ClassError("oss_delete", fmt.Errorf("%s: %w", info.Key, err))
		}
		removed++
	}
	return Result{Note: fmt.Sprintf("deleted %d collected objects older than %d days", removed, settings.Days)}, nil
}
