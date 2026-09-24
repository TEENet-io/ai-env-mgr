package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// ActionQuotaDefaults is the audit action for changing what new accounts get.
const ActionQuotaDefaults = "settings.quota_defaults"

// DefaultQuota is what an account gets when nothing else has been decided:
// the built-in fallback behind the quota-defaults setting.
var DefaultQuota = repo.Quota{MonthlyBudget: "20", RPM: litellm.NoLimitRPM, TPM: litellm.NoLimitTPM, Parallel: litellm.NoLimitParallel}

// quotaDefaultsSetting is the stored shape, kept identical to the old
// admin/quota-defaults.json so the import needs no translation.
type quotaDefaultsSetting struct {
	MonthlyBudgetUSD float64 `json:"monthlyBudgetUSD"`
	RPM              int     `json:"rpm"`
	TPM              int     `json:"tpm"`
	Parallel         int     `json:"parallel"`
}

// QuotaDefaults returns the quota a new account is pre-filled with, and the
// setting's version for a later save. Version 0 means nothing is stored and
// the built-in default was returned.
//
// Only "nothing stored" falls back. A read that failed is reported, because
// silently opening an account on the built-in numbers is opening it on a
// budget nobody chose.
func (s *Service) QuotaDefaults(ctx context.Context) (repo.Quota, int, error) {
	setting, err := s.store.Settings().Get(ctx, repo.SettingQuotaDefaults)
	if errors.Is(err, repo.ErrNotFound) {
		return DefaultQuota, 0, nil
	}
	if err != nil {
		return repo.Quota{}, 0, err
	}
	var v quotaDefaultsSetting
	if err := json.Unmarshal(setting.Value, &v); err != nil {
		return repo.Quota{}, 0, fmt.Errorf("the stored quota defaults are not readable: %w", err)
	}
	return repo.Quota{
		// Only the budget is kept; rates are never limited (litellm.BudgetOnly).
		MonthlyBudget: strconv.FormatFloat(v.MonthlyBudgetUSD, 'f', -1, 64),
		RPM:           litellm.NoLimitRPM, TPM: litellm.NoLimitTPM, Parallel: litellm.NoLimitParallel,
	}, setting.Version, nil
}

// SetQuotaDefaults stores what future accounts are pre-filled with. It does
// not touch existing accounts.
func (s *Service) SetQuotaDefaults(ctx context.Context, q repo.Quota, expectVersion int, actor, requestID string) error {
	budget, err := strconv.ParseFloat(q.MonthlyBudget, 64)
	if err != nil || !(budget > 0) || q.RPM <= 0 || q.TPM <= 0 || q.Parallel <= 0 {
		return errors.New("quota defaults: every limit must be a positive number")
	}
	value, err := json.Marshal(quotaDefaultsSetting{
		MonthlyBudgetUSD: budget, RPM: q.RPM, TPM: q.TPM, Parallel: q.Parallel,
	})
	if err != nil {
		return err
	}
	err = s.store.InTx(ctx, func(tx repo.Store) error {
		before, err := tx.Settings().Get(ctx, repo.SettingQuotaDefaults)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}
		after, err := tx.Settings().Set(ctx, repo.SettingQuotaDefaults, value, expectVersion, actor)
		if err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionQuotaDefaults, "settings", repo.SettingQuotaDefaults,
			json.RawMessage(nonEmpty(before.Value)), json.RawMessage(after.Value))
	})
	if err != nil {
		return fmt.Errorf("set quota defaults: %w", err)
	}
	return nil
}

func nonEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

// Audit actions for the alert settings.
const (
	ActionAlertSettings    = "settings.alerts"
	ActionAlertChannels    = "settings.alert_channels"
	ActionRotationSettings = "settings.rotation"
	ActionDeviceChannel    = "settings.device_channel"
	ActionAlertAck         = "alert.ack"
	ActionAlertResolve     = "alert.resolve"
)

// SaveSetting stores one JSON setting with the audit line beside it. The
// before/after in the audit are what the caller passes, so a setting that
// carries sealed secrets can be recorded without them.
func (s *Service) SaveSetting(ctx context.Context, key string, value []byte, expectVersion int, action string, before, after any, actor, requestID string) error {
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		if _, err := tx.Settings().Set(ctx, key, value, expectVersion, actor); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, action, "settings", key, before, after)
	})
	if err != nil {
		return fmt.Errorf("save setting %s: %w", key, err)
	}
	return nil
}

// AckAlert and ResolveAlert are the two things a person does to an alert;
// both leave an audit line naming who.
func (s *Service) AckAlert(ctx context.Context, id, actor, requestID string) (repo.Alert, error) {
	var out repo.Alert
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		a, err := tx.Alerts().Ack(ctx, id, actor)
		if err != nil {
			return err
		}
		out = a
		return s.auditTarget(ctx, tx, actor, requestID, ActionAlertAck, "alert", id, nil, map[string]string{"title": a.Title})
	})
	return out, err
}

func (s *Service) ResolveAlert(ctx context.Context, id, actor, requestID string) (repo.Alert, error) {
	var out repo.Alert
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		a, err := tx.Alerts().ResolveByID(ctx, id, actor)
		if err != nil {
			return err
		}
		out = a
		return s.auditTarget(ctx, tx, actor, requestID, ActionAlertResolve, "alert", id, nil, map[string]string{"title": a.Title})
	})
	return out, err
}

// LiftRateLimits moves every active employee to budget-only limits: each
// keeps its monthly budget, and requests, tokens and concurrency become
// litellm's "no limit" values. It goes through SetQuota, so each change is
// audited and pushed to the gateway by the Worker. Employees already there
// are skipped; running it twice changes nothing. Returns who was changed.
func (s *Service) LiftRateLimits(ctx context.Context, actor, requestID string) ([]string, error) {
	employees, err := s.store.Employees().List(ctx, repo.EmployeeFilter{})
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, e := range employees {
		if !e.Active() {
			continue
		}
		q, err := s.store.Quotas().Get(ctx, e.ID)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			return changed, err
		}
		if q.RPM == litellm.NoLimitRPM && q.TPM == litellm.NoLimitTPM && q.Parallel == litellm.NoLimitParallel {
			continue
		}
		next := q
		next.RPM, next.TPM, next.Parallel = litellm.NoLimitRPM, litellm.NoLimitTPM, litellm.NoLimitParallel
		if err := s.SetQuota(ctx, e.ID, next, q.Version, actor, requestID); err != nil {
			return changed, fmt.Errorf("%s: %w", e.WindowsUser, err)
		}
		changed = append(changed, e.WindowsUser)
	}
	return changed, nil
}
