package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// StatusSource lists and reads the _status/<machine> objects.
type StatusSource interface {
	Get(key string) ([]byte, string, error)
	List(prefix string) ([]string, error)
}

// StatusImport copies each machine's report from OSS into the database.
//
// It runs on a schedule for as long as agents write their status to OSS. A
// machine seen for the first time is registered, because that is how machines
// arrive: by turning up. The import never touches bindings or policy -- what a
// machine says about itself is not evidence about what it should have.
type StatusImport struct {
	Store   repo.Store
	Objects StatusSource
}

// Run imports every status object that has changed since the last pass.
func (h StatusImport) Run(ctx context.Context, _ repo.Task) (Result, error) {
	if settings, _, err := repo.LoadDeviceChannelSettings(ctx, h.Store.Settings()); err != nil {
		return Result{}, err
	} else if !settings.ImportOSSStatus {
		return Result{Note: "oss status import off"}, nil
	}
	keys, err := h.Objects.List(ossclient.StatusPrefix)
	if err != nil {
		return Result{}, ClassError("oss_list", err)
	}
	imported, skipped := 0, 0
	for _, key := range keys {
		machine := ossclient.MachineFromStatusKey(key)
		if machine == "" {
			continue
		}
		data, etag, err := h.Objects.Get(key)
		if errors.Is(err, ossclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return Result{Note: fmt.Sprintf("imported %d before OSS stopped answering", imported)},
				ClassError("oss_read", err)
		}
		var status model.Status
		if err := json.Unmarshal(data, &status); err != nil {
			// One machine's broken report must not stop the other forty from
			// being read; it stays on its previous report and is noticed by
			// its age.
			skipped++
			continue
		}

		device, err := h.Store.Devices().EnsureByHostname(ctx, machine)
		if err != nil {
			return Result{}, err
		}
		report := ReportFromStatus(device.ID, status, data, etag)
		written, err := h.Store.Reports().Import(ctx, report)
		if err != nil {
			return Result{}, err
		}
		if !written {
			skipped++
			continue
		}
		imported++
		// What the machine says about its release targets is the only
		// evidence they are ever settled on.
		if err := SettleTargets(ctx, h.Store, device.ID, status); err != nil {
			return Result{}, err
		}
		lastSeen := time.Now().UTC()
		if report.LastSyncAt != nil {
			lastSeen = *report.LastSyncAt
		}
		if err := h.Store.Devices().MarkSeen(ctx, device.ID, status.AgentVersion, lastSeen); err != nil {
			return Result{}, err
		}
	}
	return Result{Note: fmt.Sprintf("imported %d report(s), %d unchanged or unreadable", imported, skipped)}, nil
}

// ReportFromStatus maps the agent's object onto the columns the console
// filters on. The whole object rides along as well.
func ReportFromStatus(deviceID string, s model.Status, raw []byte, etag string) repo.DeviceReport {
	r := repo.DeviceReport{
		DeviceID:          deviceID,
		AgentVersion:      s.AgentVersion,
		BoundWindowsUser:  s.BoundUser,
		BoundUserExists:   &s.BoundUserExists,
		PolicyETag:        s.PolicyETag,
		CredsETag:         s.CredsETag,
		CredsApplied:      &s.CredsApplied,
		AppLockerMode:     s.AppLockerMode,
		CollectEnabled:    &s.CollectEnabled,
		CollectUploaded:   s.CollectUploaded,
		CodexVersion:      s.CodexVersion,
		CodexState:        s.CodexState,
		CodexRestartNonce: s.CodexRestartNonce,
		CodexRestartNote:  s.CodexRestartNote,
		LastEvent:         s.LastEvent,
		ErrorCount:        len(s.Errors),
		WarningCount:      len(s.Warnings),
		Report:            raw,
		SourceETag:        etag,
	}
	r.LastSyncAt = parseStamp(s.LastSync)
	r.CodexRestartAt = parseStamp(s.CodexRestartAt)
	r.LastEventAt = parseStamp(s.LastEventAt)
	return r
}

func parseStamp(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
