package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// DevicePrune forgets machines that enrolled on their own and were never
// assigned to anybody within a week: a stranger's curiosity, a test that
// was not cleaned up. A machine that was ever bound is somebody's and is
// left alone, however quiet it is -- silence is what the offline alert is
// for.
type DevicePrune struct {
	Store repo.Store
	After time.Duration // default 7 days
	Now   func() time.Time
}

func (h DevicePrune) Run(ctx context.Context, _ repo.Task) (Result, error) {
	after := h.After
	if after <= 0 {
		after = 7 * 24 * time.Hour
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	devices, err := h.Store.Devices().List(ctx, repo.DeviceFilter{})
	if err != nil {
		return Result{}, err
	}
	pruned := 0
	for _, d := range devices {
		if d.Channel != repo.ChannelAPI || d.EnrolledAt == nil || now.Sub(*d.EnrolledAt) < after {
			continue
		}
		history, err := h.Store.Bindings().History(ctx, d.ID)
		if err != nil {
			return Result{}, err
		}
		if len(history) > 0 {
			continue
		}
		err = h.Store.InTx(ctx, func(tx repo.Store) error {
			if _, err := tx.DeviceTokens().Revoke(ctx, d.ID); err != nil {
				return err
			}
			if _, err := tx.Devices().Revoke(ctx, d.ID); err != nil {
				return err
			}
			_, err := tx.Audit().Append(ctx, repo.AuditEvent{
				ActorType: "system", ActorID: "device_prune", Action: "device.prune",
				TargetType: "device", TargetID: d.ID, Result: "ok",
				After: []byte(fmt.Sprintf(`{"hostname":%q,"enrolledAt":%q}`, d.Hostname, d.EnrolledAt.UTC().Format(time.RFC3339))),
			})
			return err
		})
		if err != nil {
			return Result{}, err
		}
		pruned++
	}
	return Result{Note: fmt.Sprintf("forgot %d machine(s) nobody assigned within %s", pruned, after)}, nil
}
