package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// SettingCatalogDelivery remembers the model catalog the fleet was last given
// and the most recent delivery, so the console can say what changed on the
// gateway since and which machines have the new configuration.
const SettingCatalogDelivery = "catalog_delivery"

// ActionCatalogDeliver is the audit action for 下发模型配置.
const ActionCatalogDeliver = "models.deliver"

// CatalogSnapshot is what the gateway offered at one moment: the model
// names, and a digest over everything the picker shows about them, so a
// changed display name or context window counts as a change too.
type CatalogSnapshot struct {
	Models []string `json:"models"`
	Digest string   `json:"digest"`
}

// SnapshotOf takes a snapshot of the gateway's catalog.
func SnapshotOf(models []litellm.Model) CatalogSnapshot {
	sorted := append([]litellm.Model(nil), models...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	names := make([]string, 0, len(sorted))
	for _, m := range sorted {
		names = append(names, m.Name)
	}
	// Only what the picker shows counts as a change: where a model is routed
	// from (provider, endpoint, channel) is not the employee's business.
	type shown struct {
		Name string
		Info litellm.ModelInfo
	}
	view := make([]shown, 0, len(sorted))
	for _, m := range sorted {
		info := m.Info
		info.LitellmProvider, info.Channel = "", ""
		view = append(view, shown{m.Name, info})
	}
	data, _ := json.Marshal(view)
	sum := sha256.Sum256(data)
	return CatalogSnapshot{Models: names, Digest: hex.EncodeToString(sum[:12])}
}

// CatalogDelivery is the stored setting.
type CatalogDelivery struct {
	// Fleet is the catalog as of the last delivery to everybody. The diff
	// on the page is against it; a test delivery does not move it.
	Fleet   CatalogSnapshot `json:"fleet"`
	FleetAt string          `json:"fleetAt,omitempty"`
	FleetBy string          `json:"fleetBy,omitempty"`

	// Last is the most recent delivery, test or fleet: the one whose
	// arrival the page follows. Tasks maps employee id to the export task.
	Last LastDelivery `json:"last"`
}

// LastDelivery is one press of the button.
type LastDelivery struct {
	Scope string            `json:"scope"` // "all" or "test"
	At    string            `json:"at"`
	By    string            `json:"by"`
	Tasks map[string]string `json:"tasks"`
}

// CatalogDeliveryState reads the setting; nothing stored is a zero value
// with version 0.
func (s *Service) CatalogDeliveryState(ctx context.Context) (CatalogDelivery, int, error) {
	return s.catalogDeliveryIn(ctx, s.store)
}

// DeliverCatalog rewrites the delivered configuration -- config.toml and the
// picker's models.json -- of every active employee who holds a gateway
// token (employeeID ""), or of one employee first (a test delivery). The
// export reads the gateway's catalog when it runs, so what is delivered is
// the gateway as it is then; snapshot is what the administrator was shown,
// and becomes the fleet baseline when everybody is delivered to.
//
// It returns how many employees were queued.
func (s *Service) DeliverCatalog(ctx context.Context, snapshot CatalogSnapshot, employeeID string, expectVersion int, actor, requestID string) (int, error) {
	now := s.now()
	marker := "catalog:" + strconv.FormatInt(now.UnixNano(), 36)
	scope := "all"
	if employeeID != "" {
		scope = "test"
	}
	var queued int
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		var targets []repo.Employee
		if employeeID != "" {
			e, err := tx.Employees().ByID(ctx, employeeID)
			if err != nil {
				return err
			}
			targets = []repo.Employee{e}
		} else {
			all, err := tx.Employees().List(ctx, repo.EmployeeFilter{})
			if err != nil {
				return err
			}
			targets = all
		}
		tasks := map[string]string{}
		for _, e := range targets {
			if !e.Active() {
				if employeeID != "" {
					return fmt.Errorf("%s is not active", e.WindowsUser)
				}
				continue
			}
			if _, err := tx.Credentials().Live(ctx, e.ID, repo.PurposeCodexGateway); errors.Is(err, repo.ErrNotFound) {
				if employeeID != "" {
					return fmt.Errorf("%s has no gateway token yet; there is no configuration to deliver", e.WindowsUser)
				}
				continue
			} else if err != nil {
				return err
			}
			task, err := s.enqueueEmployeeExportTask(ctx, tx, e, marker)
			if err != nil {
				return err
			}
			tasks[e.ID] = task.ID
		}
		if len(tasks) == 0 {
			return errors.New("no active employee holds a gateway token; nothing to deliver")
		}
		queued = len(tasks)

		before, _, err := s.catalogDeliveryIn(ctx, tx)
		if err != nil {
			return err
		}
		after := before
		after.Last = LastDelivery{Scope: scope, At: now.UTC().Format(time.RFC3339), By: actor, Tasks: tasks}
		if scope == "all" {
			after.Fleet, after.FleetAt, after.FleetBy = snapshot, after.Last.At, actor
		}
		value, err := json.Marshal(after)
		if err != nil {
			return err
		}
		if _, err := tx.Settings().Set(ctx, SettingCatalogDelivery, value, expectVersion, actor); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionCatalogDeliver, "settings", SettingCatalogDelivery,
			map[string]any{"models": before.Fleet.Models},
			map[string]any{"scope": scope, "models": snapshot.Models, "employees": queued})
	})
	if err != nil {
		return 0, fmt.Errorf("deliver the model configuration: %w", err)
	}
	return queued, nil
}

func (s *Service) catalogDeliveryIn(ctx context.Context, tx repo.Store) (CatalogDelivery, int, error) {
	var d CatalogDelivery
	setting, err := tx.Settings().Get(ctx, SettingCatalogDelivery)
	if errors.Is(err, repo.ErrNotFound) {
		return d, 0, nil
	}
	if err != nil {
		return d, 0, err
	}
	if err := json.Unmarshal(setting.Value, &d); err != nil {
		return d, 0, fmt.Errorf("the stored catalog delivery is not readable: %w", err)
	}
	return d, setting.Version, nil
}
