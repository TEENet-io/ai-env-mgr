package admincore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// AccountSpec is everything an administrator decides when opening an
// account. Models nil means every model the gateway routes.
type AccountSpec struct {
	WindowsUser string
	Name        string
	Department  string
	Quota       litellm.Quota
	Models      []string

	// CodexAccount and ClaudeAccount are administrator notes -- which login
	// this Windows account uses for each tool -- not read by anything in
	// admincore. Empty leaves whatever is already on the roster alone, so
	// reopening an account (adminweb's actionAccountReopen) never blanks a
	// note nobody re-typed.
	CodexAccount  string
	ClaudeAccount string
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
	// 统一小写: a name typed into the form opens a lowercase account, so the
	// roster, the gateway alias and the object-store prefix agree without
	// anyone having to remember how they capitalised it. Only new entries are
	// affected -- Find still matches a legacy mixed-case entry, and the C1
	// rule below keeps using that entry's own spelling. Nothing is rewritten;
	// migrating stored objects is not worth the risk for a handful of rows.
	spec.WindowsUser = strings.ToLower(strings.TrimSpace(spec.WindowsUser))
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
	if spec.CodexAccount != "" {
		e.CodexAccount = spec.CodexAccount
	}
	if spec.ClaudeAccount != "" {
		e.ClaudeAccount = spec.ClaudeAccount
	}
	if err := m.SaveUsers(us); err != nil {
		return err
	}
	// Use the roster's spelling from here on: stored objects are keyed by it.
	// Find matches case-insensitively but credsKey and ossclient.UserPrefix
	// preserve case, so typing "Work1" for roster entry "work1" would publish
	// credentials.zip under a prefix the agent never reads.
	name := e.WindowsUser
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
	if err := m.provisionLocked(ctx, gw, cfg, name, allowed); err != nil {
		return err
	}

	// 5. Audit.
	m.appendAudit(name, AuditOnboard, map[string]any{
		"name": spec.Name, "department": spec.Department,
		"budget": quota.MonthlyBudgetUSD, "rpm": quota.RPM, "tpm": quota.TPM, "parallel": quota.Parallel,
		"models": allowed,
	})
	return nil
}

// Offboard closes an account: roster disabled, token revoked, delivered
// files withdrawn, machines unbound. The gateway user stays -- its spend
// history is the record of what this person used.
//
// Every step runs even if an earlier one failed. The two that matter for
// safety (disable, revoke) come first; the rest must still happen when the
// gateway is unreachable, or a departed employee's machine keeps a working
// configuration until someone remembers to try again. Failures are joined
// and reported, and the account list flags whatever is left over.
//
// A name that is not on the roster is not refused: a token whose alias
// matches nobody is precisely what the account list flags as "已离职仍有令牌",
// and its 修复 button posts here. The roster step is then skipped and the
// rest -- revoke, withdraw, unbind -- runs against the name as given.
func (m *Manager) Offboard(ctx context.Context, gw Gateway, windowsUser string) error {
	defer lockProvision(windowsUser)()

	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	// Use the roster's spelling from here on: stored objects are keyed by it.
	name := windowsUser
	if e := us.Find(windowsUser); e != nil {
		name = e.WindowsUser
		e.Enabled = false
		if err := m.SaveUsers(us); err != nil {
			return err
		}
	}

	var failures []error
	alias := KeyAlias(name)
	if _, found, err := gw.FindKeyByAlias(ctx, alias); err != nil {
		failures = append(failures, fmt.Errorf("look up token: %w", err))
	} else if found {
		if err := gw.DeleteKeyByAlias(ctx, alias); err != nil {
			failures = append(failures, fmt.Errorf("revoke token: %w", err))
		}
	}
	if err := m.Store.Delete(credsKey(name)); err != nil {
		failures = append(failures, fmt.Errorf("withdraw delivered configuration: %w", err))
	}
	if _, err := m.unbindUser(name); err != nil {
		failures = append(failures, fmt.Errorf("unbind machines: %w", err))
	}

	if len(failures) > 0 {
		joined := errors.Join(failures...)
		// Record what did happen: the roster, the files and the bindings are
		// already changed, and a history that skipped the line would read as
		// if nobody had ever tried. The flag keeps it from reading as a clean
		// close, which is what the account list's leftover tags are for.
		m.appendAudit(name, AuditOffboard, map[string]any{"partial": true, "error": joined.Error()})
		return fmt.Errorf("user %q is disabled, but: %w", name, joined)
	}
	m.appendAudit(name, AuditOffboard, nil)
	return nil
}

// requireActive returns the roster entry for windowsUser, refusing one that
// has been offboarded.
//
// Offboarding revokes the token and withdraws the delivered files on
// purpose. Re-issuing a token, or editing limits, models or labels for a
// departed employee would undo part of that silently; 重新开户 (Onboard) is
// the one path that puts somebody back in service, and it says so in the
// history.
func (m *Manager) requireActive(windowsUser string) (model.UserEntry, error) {
	us, err := m.LoadUsers()
	if err != nil {
		return model.UserEntry{}, err
	}
	e := us.Find(windowsUser)
	if e == nil {
		return model.UserEntry{}, fmt.Errorf("user %q not found in roster", windowsUser)
	}
	if !e.Enabled {
		return model.UserEntry{}, fmt.Errorf("user %q is offboarded; reopen the account first", e.WindowsUser)
	}
	return *e, nil
}

// UpdateProfile edits the labels an administrator keeps for an employee:
// display name, department and the two account notes.
//
// Every field is written verbatim, so clearing one on the form clears it on
// the roster. That is the opposite of Onboard, where an empty note means
// "leave what is there" because the row buttons post no fields for them;
// here the form is filled in from the roster and shows exactly what will be
// saved.
//
// The gateway user is mirrored (alias, department) only when it exists:
// creating one here would leave it without the limits Onboard gives it. A
// user whose stored limits are unusable is left alone rather than being
// written back with them, since UpsertUser would send them on as they are.
func (m *Manager) UpdateProfile(ctx context.Context, gw Gateway, windowsUser, name, department, codexAccount, claudeAccount string) error {
	defer lockProvision(windowsUser)()

	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	e := us.Find(windowsUser)
	if e == nil {
		return fmt.Errorf("user %q not found in roster", windowsUser)
	}
	if !e.Enabled {
		return fmt.Errorf("user %q is offboarded; reopen the account first", e.WindowsUser)
	}
	e.Name, e.Department = name, department
	e.CodexAccount, e.ClaudeAccount = codexAccount, claudeAccount
	if err := m.SaveUsers(us); err != nil {
		return err
	}
	entry := *e
	// The roster is the change being recorded and it is already durable, so
	// the line is written here rather than after the mirror, which may fail
	// on its own without making the record wrong.
	m.appendAudit(entry.WindowsUser, AuditProfile, map[string]any{"name": name, "department": department})

	id := KeyAlias(entry.WindowsUser)
	existing, found, err := gw.UserInfo(ctx, id)
	if err != nil {
		return fmt.Errorf("look up gateway user %q: %w", id, err)
	}
	if !found {
		return nil
	}
	q := existing.Quota()
	if err := q.Validate(); err != nil {
		return fmt.Errorf("profile saved, but gateway user %q has invalid limits; fix them on the account page", id)
	}
	return m.ensureGatewayUser(ctx, gw, entry, &q, existing.Models)
}

// SetQuota changes an employee's limits. It takes effect at the gateway
// immediately and touches nothing on the machine.
//
// It requires the gateway user to exist: an employee with a token but no
// user predates this feature, and the repair for that is Onboard, which
// creates the user with the right labels; quietly creating one here would
// leave it unlabelled.
func (m *Manager) SetQuota(ctx context.Context, gw Gateway, windowsUser string, q litellm.Quota) error {
	if err := q.Validate(); err != nil {
		return fmt.Errorf("quota for %q: %w", windowsUser, err)
	}
	defer lockProvision(windowsUser)()

	e, err := m.requireActive(windowsUser)
	if err != nil {
		return err
	}
	id := KeyAlias(e.WindowsUser)
	existing, found, err := gw.UserInfo(ctx, id)
	if err != nil {
		return fmt.Errorf("look up gateway user %q: %w", id, err)
	}
	if !found {
		return fmt.Errorf("user %q has no gateway account yet; open one first", e.WindowsUser)
	}
	if err := m.ensureGatewayUser(ctx, gw, e, &q, existing.Models); err != nil {
		return err
	}
	m.appendAudit(e.WindowsUser, AuditQuota, map[string]any{
		"budget": q.MonthlyBudgetUSD, "rpm": q.RPM, "tpm": q.TPM, "parallel": q.Parallel,
	})
	return nil
}

// SetModels changes which models an employee may use, on the user, on the
// token and in the delivered catalog. All three are needed: the user and
// token govern what they can reach, the catalog what they can see.
func (m *Manager) SetModels(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) error {
	defer lockProvision(windowsUser)()
	if _, err := m.requireActive(windowsUser); err != nil {
		return err
	}
	allowed, err := m.setModelsLocked(ctx, gw, cfg, windowsUser, models)
	if err != nil {
		return err
	}
	m.appendAudit(windowsUser, AuditModels, map[string]any{"models": allowed})
	return nil
}

// Reissue replaces an employee's token and redelivers the configuration.
// The quota is the user's and is untouched.
func (m *Manager) Reissue(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string) error {
	defer lockProvision(windowsUser)()
	if _, err := m.requireActive(windowsUser); err != nil {
		return err
	}
	if err := m.provisionLocked(ctx, gw, cfg, windowsUser, nil); err != nil {
		return err
	}
	m.appendAudit(windowsUser, AuditReissue, nil)
	return nil
}

// unbindUser removes every machine binding that points at windowsUser.
// Windows account names are case-insensitive, so the match is too.
//
// Every matching binding is attempted even if an earlier one fails to
// delete: one machine's transient store error must not leave every other
// machine still pointing at a departed employee. n counts the deletions
// that succeeded; any failures are joined and returned alongside it.
func (m *Manager) unbindUser(windowsUser string) (int, error) {
	bindings, err := m.ListBindings()
	if err != nil {
		return 0, err
	}
	var failures []error
	n := 0
	for machine, b := range bindings {
		if !strings.EqualFold(b.User, windowsUser) {
			continue
		}
		if err := m.Store.Delete(ossclient.BindingKey(machine)); err != nil {
			failures = append(failures, fmt.Errorf("unbind %q: %w", machine, err))
			continue
		}
		n++
	}
	return n, errors.Join(failures...)
}
