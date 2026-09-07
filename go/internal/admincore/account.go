package admincore

import (
	"context"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// AccountSpec is everything an administrator decides when opening an
// account. Models nil means every model the gateway routes.
type AccountSpec struct {
	WindowsUser string
	Name        string
	Department  string
	Quota       litellm.Quota
	Models      []string
}

// Onboard opens an account: roster entry, gateway user with its limits, a
// token under that user, and the Codex configuration delivered to the
// employee's machine. It is idempotent -- running it again for the same
// person updates the labels and quota, revokes the previous token and
// issues a new one.
//
// Order matters and is chosen so that a failure at any step leaves a state
// the account list can name (see adminweb reconcileAccounts) and a re-run
// repairs: the roster first (cheap, local), then the user (the token needs
// an owner), then the token, then delivery. A delivery failure withdraws
// the token just minted so nothing live is left unreferenced.
func (m *Manager) Onboard(ctx context.Context, gw Gateway, cfg GatewayConfig, spec AccountSpec) error {
	if spec.WindowsUser == "" {
		return fmt.Errorf("onboard: a Windows user name is required")
	}
	if err := spec.Quota.Validate(); err != nil {
		return fmt.Errorf("onboard %q: %w", spec.WindowsUser, err)
	}
	if cfg.BaseURL == "" {
		return fmt.Errorf("gateway base URL is not configured")
	}
	defer lockProvision(spec.WindowsUser)()

	// 1. Roster.
	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	e := us.Find(spec.WindowsUser)
	if e == nil {
		us.Users = append(us.Users, model.UserEntry{WindowsUser: spec.WindowsUser})
		e = &us.Users[len(us.Users)-1]
	}
	e.Name, e.Department, e.Enabled = spec.Name, spec.Department, true
	if err := m.SaveUsers(us); err != nil {
		return err
	}
	entry := *e

	// 2. Gateway user, with the quota the administrator chose. The
	// allowlist is validated against the gateway first so a typo is
	// reported before anything is written there.
	available, err := gw.Models(ctx)
	if err != nil {
		return fmt.Errorf("read gateway model catalog: %w", err)
	}
	allowed, err := resolveAllowlist(available, spec.Models)
	if err != nil {
		return err
	}
	quota := spec.Quota
	if err := m.ensureGatewayUser(ctx, gw, entry, &quota, allowed); err != nil {
		return err
	}

	// 3 + 4. Token and delivery, shared with re-issuing.
	if err := m.provisionLocked(ctx, gw, cfg, spec.WindowsUser, allowed); err != nil {
		return err
	}

	// 5. Audit.
	m.appendAudit(spec.WindowsUser, AuditOnboard, map[string]any{
		"name": spec.Name, "department": spec.Department,
		"budget": quota.MonthlyBudgetUSD, "rpm": quota.RPM, "tpm": quota.TPM, "parallel": quota.Parallel,
		"models": allowed,
	})
	return nil
}
