package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// CredentialRotation replaces gateway tokens that have reached their
// maximum age, a few a night, and only for people whose machine will pick
// the new one up. It is the ordinary re-issue, started by a clock instead
// of a person: the old token is revoked, a new one minted and delivered,
// Codex on the machine keeps working.
//
// The activity check is the safety: revoking the token of a machine that
// is not syncing cuts that person off until it does, with nobody the wiser.
// Such a token is left alone and named in the note, and the offline-machine
// alert is what says why.
type CredentialRotation struct {
	Store repo.Store
	Ops   *ops.Service
	Now   func() time.Time
}

func (h CredentialRotation) Run(ctx context.Context, _ repo.Task) (Result, error) {
	settings, _, err := repo.LoadRotationSettings(ctx, h.Store.Settings())
	if err != nil {
		return Result{}, err
	}
	if !settings.Enabled {
		return Result{Note: "rotation is off"}, nil
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	due, err := h.Store.Credentials().LiveOlderThan(ctx, repo.PurposeCodexGateway,
		now.AddDate(0, 0, -settings.MaxAgeDays), settings.PerDay*4)
	if err != nil {
		return Result{}, err
	}
	rotated, left := 0, 0
	var skipped []string
	for _, c := range due {
		employee, err := h.Store.Employees().ByID(ctx, c.EmployeeID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				continue
			}
			return Result{}, err
		}
		if !employee.Active() {
			// Offboarding retires the credential itself; a live one on a
			// closed account is a task that has not run yet.
			continue
		}
		if c.Epoch < employee.AuthEpoch {
			// A replacement is already under way: the epoch moved and the
			// provisioning that retires this credential has not run yet.
			// Rotating again would move it a second time for nothing.
			continue
		}
		if reason := h.notReady(ctx, employee, now, settings); reason != "" {
			skipped = append(skipped, employee.WindowsUser+" ("+reason+")")
			continue
		}
		if rotated >= settings.PerDay {
			left++
			continue
		}
		if _, err := h.Ops.Rotate(ctx, employee.ID, "rotation", ""); err != nil {
			skipped = append(skipped, employee.WindowsUser+" ("+err.Error()+")")
			continue
		}
		rotated++
	}
	note := fmt.Sprintf("rotated %d", rotated)
	if len(skipped) > 0 {
		note += fmt.Sprintf(", skipped %d: %s", len(skipped), strings.Join(skipped, ", "))
	}
	if left > 0 {
		note += fmt.Sprintf(", %d more due (daily limit %d)", left, settings.PerDay)
	}
	return Result{Note: note}, nil
}

// notReady says why an employee is not rotated tonight, or "" to go ahead:
// no machine, or none that has reported within the window.
func (h CredentialRotation) notReady(ctx context.Context, employee repo.Employee, now time.Time, s repo.RotationSettings) string {
	bindings, err := h.Store.Bindings().OpenByEmployee(ctx, employee.ID)
	if err != nil || len(bindings) == 0 {
		return "no machine"
	}
	limit := now.Add(-time.Duration(s.ActiveWithinHours) * time.Hour)
	for _, b := range bindings {
		d, err := h.Store.Devices().ByID(ctx, b.DeviceID)
		if err == nil && d.LastSeenAt != nil && d.LastSeenAt.After(limit) {
			return ""
		}
	}
	return "machine silent"
}
