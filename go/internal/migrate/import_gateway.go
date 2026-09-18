package migrate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Gateway is the part of LiteLLM the import reads. Nothing here writes to it.
type Gateway interface {
	ListUsers(ctx context.Context) ([]litellm.User, error)
	ListKeys(ctx context.Context) ([]litellm.Key, error)
}

// legacyUserID is how the old console named an employee on the gateway, and
// legacyAlias how it named their one token. Both are "emp-<windows user>".
// The user id is kept by the new code; the alias is not, which is why the
// import has to record it -- it is the only handle a revoke has.
func legacyUserID(windowsUser string) string { return "emp-" + strings.ToLower(windowsUser) }

// importGateway brings in what the gateway holds for each employee: their
// limits, their model allowlist, and the token they are using today.
//
// Without this an imported employee has no quota (so provisioning fails) and
// no grant (so offboarding revokes nothing, and the token they walked in with
// keeps working after they walk out). The token itself cannot be imported --
// its plaintext is gone -- so the grant carries the alias and no credential.
func (im *Importer) importGateway(ctx context.Context, tx repo.Store, byUser map[string]repo.Employee, report *Report) error {
	if im.Gateway == nil {
		report.Warnings = append(report.Warnings,
			"no gateway configured: quotas, models and existing tokens were not imported; "+
				"offboarding an imported employee would revoke nothing")
		return nil
	}
	users, err := im.Gateway.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list gateway users: %w", err)
	}
	keys, err := im.Gateway.ListKeys(ctx)
	if err != nil {
		return fmt.Errorf("list gateway keys: %w", err)
	}
	userByID := make(map[string]litellm.User, len(users))
	for _, u := range users {
		userByID[u.UserID] = u
	}
	keysByUser := map[string][]litellm.Key{}
	for _, k := range keys {
		owner := k.UserID
		if owner == "" {
			// Older keys were minted with the alias as the user id and no
			// owner recorded; the alias says whose it is.
			owner = k.KeyAlias
		}
		keysByUser[owner] = append(keysByUser[owner], k)
	}

	for windowsUser, employee := range byUser {
		userID := legacyUserID(windowsUser)
		user, known := userByID[userID]
		ownedKeys := keysByUser[userID]

		if known {
			if err := im.importQuota(ctx, tx, employee, user, report); err != nil {
				return err
			}
		} else if employee.Active() {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"%s is on the roster but has no gateway user; they have no quota and cannot be provisioned until one is set",
				windowsUser))
		}

		models := user.Models
		if len(ownedKeys) > 0 && len(ownedKeys[0].Models) > 0 {
			// The key's own list governs on the gateway.
			models = ownedKeys[0].Models
		}
		if err := tx.Employees().SetModels(ctx, employee.ID, models); err != nil {
			return err
		}

		if err := im.importGrants(ctx, tx, employee, userID, ownedKeys, report); err != nil {
			return err
		}
	}

	// Tokens on the gateway that belong to nobody on the roster are exactly
	// what an audit wants to hear about.
	for owner, ks := range keysByUser {
		if !strings.HasPrefix(owner, "emp-") {
			continue
		}
		if _, ok := byUser[strings.TrimPrefix(owner, "emp-")]; ok {
			continue
		}
		for _, k := range ks {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"the gateway holds token %q for %s, who is not on the roster", k.KeyAlias, owner))
		}
	}
	return nil
}

func (im *Importer) importQuota(ctx context.Context, tx repo.Store, employee repo.Employee, user litellm.User, report *Report) error {
	q := user.Quota()
	if !(q.MonthlyBudgetUSD > 0) || q.RPM <= 0 || q.TPM <= 0 || q.Parallel <= 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"%s's gateway limits are incomplete (%v); not imported, set them in the console",
			employee.WindowsUser, q))
		return nil
	}
	quota := repo.Quota{
		MonthlyBudget: strconv.FormatFloat(q.MonthlyBudgetUSD, 'f', -1, 64),
		RPM:           q.RPM, TPM: q.TPM, Parallel: q.Parallel,
	}
	current, err := tx.Quotas().Get(ctx, employee.ID)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		if _, err := tx.Quotas().Set(ctx, employee.ID, quota, 0); err != nil {
			return err
		}
		report.Quotas++
	case err != nil:
		return err
	default:
		if current.MonthlyBudget != quota.MonthlyBudget || current.RPM != quota.RPM ||
			current.TPM != quota.TPM || current.Parallel != quota.Parallel {
			if _, err := tx.Quotas().Set(ctx, employee.ID, quota, current.Version); err != nil {
				return err
			}
		}
	}
	return nil
}

// importGrants records each token the gateway holds for this employee.
//
// One of them becomes the active grant; any further ones are recorded as
// unwanted, because the design allows one live token per person and a second
// one is precisely the kind of thing that should be revoked at the next
// re-issue. It is reported so nobody is surprised when that happens.
func (im *Importer) importGrants(ctx context.Context, tx repo.Store, employee repo.Employee, userID string, keys []litellm.Key, report *Report) error {
	for i, k := range keys {
		alias := k.KeyAlias
		if alias == "" {
			alias = k.Token
		}
		if alias == "" {
			continue
		}
		if _, err := tx.Grants().ByKeyAlias(ctx, "", alias); err == nil {
			continue // already recorded
		} else if !errors.Is(err, repo.ErrNotFound) {
			return err
		}

		grant, err := tx.Grants().Create(ctx, repo.NewGrant{
			EmployeeID: employee.ID, Epoch: employee.AuthEpoch,
			ExternalUser: userID, KeyAlias: alias, Models: k.Models,
		})
		if err != nil {
			return fmt.Errorf("record token %s: %w", alias, err)
		}
		report.Grants++
		wanted := employee.Active() && i == 0
		if !wanted {
			if _, err := tx.Grants().Revoke(ctx, grant.ID); err != nil {
				return err
			}
			if employee.Active() {
				report.Warnings = append(report.Warnings, fmt.Sprintf(
					"%s has more than one token on the gateway; %q is recorded as unwanted and will be revoked at the next re-issue",
					employee.WindowsUser, alias))
			}
		}
		// Observed: the listing just said it exists.
		if _, err := tx.Grants().RecordActual(ctx, grant.ID, repo.ActualActive, ""); err != nil {
			return err
		}
	}
	return nil
}
