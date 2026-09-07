package admincore

import "github.com/TEENet-io/ai-env-mgr/internal/litellm"

// LoadQuotaDefaults returns the quota a new account starts with. Task 4
// backs this with admin/quota-defaults.json; until then it is the built-in.
func (m *Manager) LoadQuotaDefaults() (litellm.Quota, error) { return DefaultQuota, nil }
