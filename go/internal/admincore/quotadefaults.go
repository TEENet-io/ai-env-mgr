package admincore

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// QuotaDefaultsKey is where the console keeps the quota it pre-fills for a
// new account. It lives under admin/ like the roster: employees' machines
// never need it.
func QuotaDefaultsKey() string { return ossclient.AdminKey("quota-defaults.json") }

// quotaDefaultsFile is the on-disk shape; field names are stable so the
// object stays readable by hand.
type quotaDefaultsFile struct {
	MonthlyBudgetUSD float64 `json:"monthlyBudgetUSD"`
	RPM              int     `json:"rpm"`
	TPM              int     `json:"tpm"`
	Parallel         int     `json:"parallel"`
}

// LoadQuotaDefaults returns the quota a new account starts with. A missing
// or unreadable object yields the built-in defaults: the first run has no
// file, and a corrupt one should not stop anyone being onboarded.
func (m *Manager) LoadQuotaDefaults() (litellm.Quota, error) {
	data, _, err := m.Store.Get(QuotaDefaultsKey())
	if err != nil {
		// Nothing stored yet -> the built-in default. Any other failure is
		// reported: onboarding falls back to these numbers when the form
		// leaves the quota blank, and silently using the built-ins would
		// open an account on a budget nobody chose.
		if errors.Is(err, ossclient.ErrNotFound) {
			return DefaultQuota, nil
		}
		return litellm.Quota{}, fmt.Errorf("read quota defaults: %w", err)
	}
	var f quotaDefaultsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return litellm.Quota{}, fmt.Errorf("parse quota defaults: %w", err)
	}
	// Only the budget is kept from the file; see litellm.BudgetOnly.
	q := litellm.BudgetOnly(f.MonthlyBudgetUSD)
	if err := q.Validate(); err != nil {
		return litellm.Quota{}, fmt.Errorf("stored quota defaults are invalid: %w", err)
	}
	return q, nil
}

// SaveQuotaDefaults stores the quota future accounts will be pre-filled
// with. It does not touch existing accounts.
func (m *Manager) SaveQuotaDefaults(q litellm.Quota) error {
	if err := q.Validate(); err != nil {
		return fmt.Errorf("quota defaults: %w", err)
	}
	data, err := json.MarshalIndent(quotaDefaultsFile{
		MonthlyBudgetUSD: q.MonthlyBudgetUSD, RPM: q.RPM, TPM: q.TPM, Parallel: q.Parallel,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quota defaults: %w", err)
	}
	if err := m.Store.Put(QuotaDefaultsKey(), data); err != nil {
		return fmt.Errorf("save quota defaults: %w", err)
	}
	return nil
}
