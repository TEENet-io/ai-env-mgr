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
	// Recent completed presses remain inspectable after a new delivery.
	History []LastDelivery `json:"history,omitempty"`
}

// LastDelivery is one press of the button.
type LastDelivery struct {
	Scope    string                  `json:"scope"` // "all", "test" or "batch"
	At       string                  `json:"at"`
	By       string                  `json:"by"`
	Snapshot CatalogSnapshot         `json:"snapshot,omitempty"`
	Targets  []CatalogDeliveryTarget `json:"targets,omitempty"`
	Tasks    map[string]string       `json:"tasks"`
	Retries  map[string]int          `json:"retries,omitempty"`
}

type CatalogDeliveryTarget struct {
	EmployeeID  string `json:"employeeId"`
	WindowsUser string `json:"windowsUser"`
	DeviceID    string `json:"deviceId,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
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
	scope := "all"
	employees := []string(nil)
	if employeeID != "" {
		scope, employees = "test", []string{employeeID}
	}
	return s.deliverCatalog(ctx, snapshot, scope, employees, nil, expectVersion, actor, requestID)
}

// DeliverCatalogBatch accepts explicit employees and/or bound devices. A
// credential bundle is employee-scoped: selecting a device also refreshes any
// other machine bound to that employee, though only selected devices are
// listed as batch targets.
func (s *Service) DeliverCatalogBatch(ctx context.Context, snapshot CatalogSnapshot, employeeIDs, deviceIDs []string, expectVersion int, actor, requestID string) (int, error) {
	if len(employeeIDs)+len(deviceIDs) == 0 || len(employeeIDs)+len(deviceIDs) > 200 {
		return 0, errors.New("select 1 to 200 employees or devices")
	}
	return s.deliverCatalog(ctx, snapshot, "batch", employeeIDs, deviceIDs, expectVersion, actor, requestID)
}

func (s *Service) deliverCatalog(ctx context.Context, snapshot CatalogSnapshot, scope string, employeeIDs, deviceIDs []string, expectVersion int, actor, requestID string) (int, error) {
	now := s.now()
	marker := "catalog:" + strconv.FormatInt(now.UnixNano(), 36)
	var queued int
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		var targets []repo.Employee
		selectedDevices := map[string]map[string]bool{}
		selectedEmployees := map[string]bool{}
		if scope == "all" {
			all, err := tx.Employees().List(ctx, repo.EmployeeFilter{})
			if err != nil {
				return err
			}
			targets = all
		} else {
			for _, id := range employeeIDs {
				if id == "" {
					return errors.New("employee selection contains an empty ID")
				}
				selectedEmployees[id] = true
			}
			for _, id := range deviceIDs {
				device, err := tx.Devices().ByID(ctx, id)
				if err != nil {
					return fmt.Errorf("selected device %s: %w", id, err)
				}
				if device.Status == repo.DeviceRevoked {
					return fmt.Errorf("%s is revoked", device.Hostname)
				}
				binding, err := tx.Bindings().Open(ctx, id)
				if err != nil {
					return fmt.Errorf("%s has no bound employee: %w", device.Hostname, err)
				}
				if selectedDevices[binding.EmployeeID] == nil {
					selectedDevices[binding.EmployeeID] = map[string]bool{}
				}
				selectedDevices[binding.EmployeeID][id] = true
			}
			ids := map[string]bool{}
			for id := range selectedEmployees {
				ids[id] = true
			}
			for id := range selectedDevices {
				ids[id] = true
			}
			for id := range ids {
				e, err := tx.Employees().ByID(ctx, id)
				if err != nil {
					return fmt.Errorf("selected employee %s: %w", id, err)
				}
				targets = append(targets, e)
			}
		}
		tasks := map[string]string{}
		var deliveryTargets []CatalogDeliveryTarget
		for _, e := range targets {
			if !e.Active() {
				if scope != "all" {
					return fmt.Errorf("%s is not active", e.WindowsUser)
				}
				continue
			}
			if _, err := tx.Credentials().Live(ctx, e.ID, repo.PurposeCodexGateway); errors.Is(err, repo.ErrNotFound) {
				if scope != "all" {
					return fmt.Errorf("%s has no gateway token yet; there is no configuration to deliver", e.WindowsUser)
				}
				continue
			} else if err != nil {
				return err
			}
			bindings, err := tx.Bindings().OpenByEmployee(ctx, e.ID)
			if err != nil {
				return err
			}
			found := false
			seenDevices := map[string]bool{}
			for _, binding := range bindings {
				if scope == "batch" && !selectedEmployees[e.ID] && !selectedDevices[e.ID][binding.DeviceID] {
					continue
				}
				device, err := tx.Devices().ByID(ctx, binding.DeviceID)
				if err != nil {
					return err
				}
				deliveryTargets = append(deliveryTargets, CatalogDeliveryTarget{EmployeeID: e.ID, WindowsUser: e.WindowsUser, DeviceID: device.ID, Hostname: device.Hostname})
				seenDevices[device.ID] = true
				found = true
			}
			for id := range selectedDevices[e.ID] {
				if !seenDevices[id] {
					return fmt.Errorf("device %s changed binding while selecting targets: %w", id, repo.ErrConflict)
				}
			}
			if !found {
				deliveryTargets = append(deliveryTargets, CatalogDeliveryTarget{EmployeeID: e.ID, WindowsUser: e.WindowsUser})
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
		if before.Last.At != "" {
			after.History = append(after.History, before.Last)
			if len(after.History) > 10 {
				after.History = after.History[len(after.History)-10:]
			}
		}
		after.Last = LastDelivery{Scope: scope, At: now.UTC().Format(time.RFC3339), By: actor, Snapshot: snapshot, Targets: deliveryTargets, Tasks: tasks}
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

// RetryCatalogDelivery requeues only one employee from the latest batch. The
// gateway must still match that batch's snapshot, otherwise a retry could
// silently deliver a different model catalog under the old result row.
func (s *Service) RetryCatalogDelivery(ctx context.Context, snapshot CatalogSnapshot, employeeID string, expectVersion int, actor, requestID string) error {
	if employeeID == "" {
		return errors.New("select an employee to retry")
	}
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		state, _, err := s.catalogDeliveryIn(ctx, tx)
		if err != nil {
			return err
		}
		if state.Last.Tasks[employeeID] == "" {
			return errors.New("employee is not in the latest delivery")
		}
		previousTask := state.Last.Tasks[employeeID]
		previous, err := tx.Tasks().ByID(ctx, previousTask)
		if err != nil {
			return err
		}
		if previous.Status != repo.TaskFailed && previous.Status != repo.TaskSuperseded && previous.Status != repo.TaskSucceeded {
			return errors.New("the previous delivery is still running; wait for its result")
		}
		if state.Last.Snapshot.Digest == "" || state.Last.Snapshot.Digest != snapshot.Digest {
			return errors.New("gateway models have changed; create a new delivery instead of retrying")
		}
		e, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		if !e.Active() {
			return fmt.Errorf("%s is no longer active", e.WindowsUser)
		}
		if _, err := tx.Credentials().Live(ctx, e.ID, repo.PurposeCodexGateway); err != nil {
			return fmt.Errorf("%s has no usable gateway token: %w", e.WindowsUser, err)
		}
		marker := "catalog-retry:" + strconv.FormatInt(s.now().UnixNano(), 36)
		task, err := s.enqueueEmployeeExportTask(ctx, tx, e, marker)
		if err != nil {
			return err
		}
		state.Last.Tasks[employeeID] = task.ID
		if state.Last.Retries == nil {
			state.Last.Retries = map[string]int{}
		}
		state.Last.Retries[employeeID]++
		value, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := tx.Settings().Set(ctx, SettingCatalogDelivery, value, expectVersion, actor); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionCatalogDeliver, "employee", employeeID,
			map[string]any{"task": previousTask}, map[string]any{"retry": state.Last.Retries[employeeID], "task": task.ID})
	})
	if err != nil {
		return fmt.Errorf("retry model delivery: %w", err)
	}
	return nil
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
