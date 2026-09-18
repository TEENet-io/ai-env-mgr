package ops

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// ActionForget is the audit action for a decommissioned machine.
const ActionForget = "machine.forget"

// ForgetMachine retires a machine that is actually gone: its binding is
// closed, the device is marked revoked, and the export removes both its
// binding and its status object so it disappears from the console.
//
// It does not delete the row. The bindings and the audit trail are the record
// of what that machine had, and a machine that is still switched on will
// simply turn up again on its next sync -- which is the console's cue that
// somebody forgot the wrong one.
func (s *Service) ForgetMachine(ctx context.Context, hostname, actor, requestID string) error {
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		device, err := tx.Devices().ByHostname(ctx, hostname)
		if err != nil {
			return err
		}
		var former string
		binding, err := tx.Bindings().Unbind(ctx, device.ID, actor)
		switch {
		case err == nil:
			former = binding.EmployeeID
			employee, err := tx.Employees().ByID(ctx, binding.EmployeeID)
			if err != nil {
				return err
			}
			if err := s.enqueueEmployeeExport(ctx, tx, employee,
				"forget:"+device.ID+":"+strconv.Itoa(binding.Epoch)); err != nil {
				return err
			}
		case !errors.Is(err, repo.ErrNotFound):
			return err
		}
		if _, err := tx.Devices().Revoke(ctx, device.ID); err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device, "forget:"+s.now().Format("20060102T150405")); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionForget, "device", device.ID,
			map[string]any{"hostname": device.Hostname, "employee": former},
			map[string]any{"hostname": device.Hostname, "status": repo.DeviceRevoked})
	})
	if err != nil {
		return fmt.Errorf("forget machine %s: %w", hostname, err)
	}
	return nil
}
