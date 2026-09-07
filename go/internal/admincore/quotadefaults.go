package admincore

import (
	"encoding/json"
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
		return DefaultQuota, nil
	}
	var f quotaDefaultsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return DefaultQuota, nil
	}
	q := litellm.Quota{MonthlyBudgetUSD: f.MonthlyBudgetUSD, RPM: f.RPM, TPM: f.TPM, Parallel: f.Parallel}
	if err := q.Validate(); err != nil {
		return DefaultQuota, nil
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
