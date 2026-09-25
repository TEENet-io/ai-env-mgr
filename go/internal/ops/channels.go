package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// ActionChannelPause and ActionChannelResume are the audit actions for the
// overview's 渠道 panel.
const (
	ActionChannelPause  = "gateway.channel_pause"
	ActionChannelResume = "gateway.channel_resume"
)

// SetChannelPaused pauses or resumes one supplier channel. The setting is
// what the exports and the gateway provisioning read; this also queues a
// gateway provisioning for every active employee holding a token who is
// allowed "every model", so their allowlist on the gateway follows the
// pause at once. Rewriting the pickers is the caller's next step
// (DeliverCatalog to everybody), because it needs the gateway's catalog.
//
// It returns how many employees' allowlists were queued.
func (s *Service) SetChannelPaused(ctx context.Context, channel string, paused bool, reason string, expectVersion int, actor, requestID string) (int, error) {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return 0, errors.New("choose a channel")
	}
	now := s.now()
	marker := "channel:" + channel + ":" + strconv.FormatInt(now.UnixNano(), 36)
	var queued int
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		current, version, err := repo.LoadGatewayChannels(ctx, tx.Settings())
		if err != nil {
			return err
		}
		if version != expectVersion {
			return fmt.Errorf("%w: the channels were changed by someone else; reload and try again", repo.ErrConflict)
		}
		_, was := current.Paused[channel]
		if was == paused {
			if paused {
				return fmt.Errorf("%s is already paused", channel)
			}
			return fmt.Errorf("%s is not paused", channel)
		}
		before := current.PausedSet()
		if paused {
			current.Paused[channel] = repo.ChannelPause{At: now.UTC().Format(time.RFC3339), By: actor, Reason: strings.TrimSpace(reason)}
		} else {
			delete(current.Paused, channel)
		}
		value, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if _, err := tx.Settings().Set(ctx, repo.SettingGatewayChannels, value, version, actor); err != nil {
			return err
		}

		employees, err := tx.Employees().List(ctx, repo.EmployeeFilter{})
		if err != nil {
			return err
		}
		for _, e := range employees {
			if !e.Active() {
				continue
			}
			models, err := tx.Employees().Models(ctx, e.ID)
			if err != nil {
				return err
			}
			if len(models) > 0 {
				continue // an explicit list is the employee's own; the picker hides the channel
			}
			if _, err := tx.Credentials().Live(ctx, e.ID, repo.PurposeCodexGateway); errors.Is(err, repo.ErrNotFound) {
				continue
			} else if err != nil {
				return err
			}
			if err := s.enqueueGatewayWork(ctx, tx, e, repo.TaskGatewayProvision, marker); err != nil {
				return err
			}
			queued++
		}
		action := ActionChannelResume
		if paused {
			action = ActionChannelPause
		}
		return s.auditTarget(ctx, tx, actor, requestID, action, "settings", repo.SettingGatewayChannels,
			map[string]any{"paused": before},
			map[string]any{"channel": channel, "paused": current.PausedSet(), "reason": strings.TrimSpace(reason), "allowlists_queued": queued})
	})
	if err != nil {
		return 0, fmt.Errorf("set channel %s: %w", channel, err)
	}
	return queued, nil
}
