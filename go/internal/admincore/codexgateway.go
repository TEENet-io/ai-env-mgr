package admincore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/TEENet-io/ai-env-mgr/internal/catalog"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// Gateway is the subset of the LiteLLM client this package needs. Taking it
// as an interface keeps the onboarding logic testable without a live
// gateway, matching how Store is handled for the object store.
type Gateway interface {
	GenerateKey(ctx context.Context, alias, userID string, models []string, metadata map[string]string) (litellm.Key, error)
	UpdateKey(ctx context.Context, key string, models []string) error
	DeleteKey(ctx context.Context, handles ...string) error
	DeleteKeyByAlias(ctx context.Context, alias string) error
	FindKeyByAlias(ctx context.Context, alias string) (litellm.Key, bool, error)
	ListKeys(ctx context.Context) ([]litellm.Key, error)
	Models(ctx context.Context) ([]litellm.Model, error)

	// Internal users carry the budget and rate limits a key inherits.
	UpsertUser(ctx context.Context, spec litellm.UserSpec) error
	UserInfo(ctx context.Context, userID string) (litellm.User, bool, error)
	ListUsers(ctx context.Context) ([]litellm.User, error)
}

// provisionLocks serialises gateway provisioning per employee.
//
// Two concurrent provisions for one person -- a double-click is enough --
// interleave badly: the second finds no token (the first has just revoked
// the old one), skips revocation, and then collides with the token the
// first is minting. The first succeeds; the second reports a confusing
// "alias already exists". Holding a per-user lock across the whole sequence
// removes the interleaving instead of papering over its symptom.
var provisionLocks sync.Map // windowsUser (lower-cased) -> *sync.Mutex

func lockProvision(windowsUser string) func() {
	v, _ := provisionLocks.LoadOrStore(strings.ToLower(windowsUser), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// GatewayConfig is what the delivered config.toml must point at.
type GatewayConfig struct {
	BaseURL string // e.g. https://litellm.teenet.app
}

// KeyAlias derives the gateway alias for an employee.
//
// It must be derived, not chosen: the gateway enforces uniqueness on the
// alias, so a deterministic value is what stops a double submission from
// issuing two live tokens, and it is the only handle for reconciling the
// gateway's token list against the roster.
func KeyAlias(windowsUser string) string { return "emp-" + strings.ToLower(windowsUser) }

// DefaultQuota is what an account gets when nothing else has been decided:
// the built-in fallback behind admin/quota-defaults.json (see
// quotadefaults.go) and the quota given to a user created on the fly while
// re-issuing a token for an employee who predates user records.
var DefaultQuota = litellm.Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4}

// ensureGatewayUser makes sure the gateway user for e exists.
//
// With quota nil an existing user is left exactly as is -- re-issuing a
// token must not silently reset a budget an administrator tuned -- and a
// missing one is created with the stored defaults. With quota set the user
// is created or updated to match: that is the onboarding and quota-change
// path.
func (m *Manager) ensureGatewayUser(ctx context.Context, gw Gateway, e model.UserEntry, quota *litellm.Quota, models []string) error {
	id := KeyAlias(e.WindowsUser)
	existing, found, err := gw.UserInfo(ctx, id)
	if err != nil {
		return fmt.Errorf("look up gateway user %q: %w", id, err)
	}
	if found && quota == nil {
		return nil
	}
	spec := litellm.UserSpec{UserID: id, Alias: e.Name, Department: e.Department, Models: models}
	switch {
	case quota != nil:
		spec.Quota = *quota
	default:
		q, err := m.LoadQuotaDefaults()
		if err != nil {
			return err
		}
		spec.Quota = q
	}
	if spec.Alias == "" {
		spec.Alias = e.WindowsUser
	}
	if found && models == nil {
		spec.Models = existing.Models
	}
	if err := gw.UpsertUser(ctx, spec); err != nil {
		return fmt.Errorf("write gateway user %q: %w", id, err)
	}
	return nil
}

// ProvisionCodexGateway issues a gateway token for one employee, under their
// gateway user, and delivers the Codex configuration that uses it. Budget
// and rate limits are the user's, so they are untouched here.
//
// The order is deliberate: the token is minted first, then written to the
// object store. Reversed, a failure between the two would leave a
// configuration on the employee's machine pointing at a token that does not
// exist -- Codex would start and fail on every request, which is harder to
// diagnose than never having been configured.
//
// Re-provisioning an employee revokes their previous token. That is a
// deliberate choice over inventing a fresh alias: a second live token for
// one person cannot be attributed or reconciled, and the old one would stay
// valid forever.
func (m *Manager) ProvisionCodexGateway(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) error {
	defer lockProvision(windowsUser)()
	return m.provisionLocked(ctx, gw, cfg, windowsUser, models)
}

// provisionLocked is the body of ProvisionCodexGateway without the lock, so
// that a caller already holding lockProvision for windowsUser (Onboard) can
// run it without deadlocking on its own lock.
func (m *Manager) provisionLocked(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) error {
	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	if us.Find(windowsUser) == nil {
		return fmt.Errorf("user %q not found in roster", windowsUser)
	}
	if cfg.BaseURL == "" {
		return fmt.Errorf("gateway base URL is not configured")
	}

	// A nil request means "leave the allowlist as it is". For a brand-new
	// user that is "everything the gateway offers" (resolveAllowlist's own
	// default); for one that already has a narrowed allowlist, re-issuing a
	// token must not widen it back out.
	if models == nil {
		if u, found, err := gw.UserInfo(ctx, KeyAlias(windowsUser)); err != nil {
			return fmt.Errorf("look up gateway user: %w", err)
		} else if found && len(u.Models) > 0 {
			models = u.Models
		}
	}

	available, err := gw.Models(ctx)
	if err != nil {
		return fmt.Errorf("read gateway model catalog: %w", err)
	}
	allowed, err := resolveAllowlist(available, models)
	if err != nil {
		return err
	}

	if err := m.ensureGatewayUser(ctx, gw, *us.Find(windowsUser), nil, nil); err != nil {
		return err
	}

	// Revoke by alias, not by the token from the listing: the listing carries
	// only the hash, and a first version of this code passed the (empty)
	// plaintext instead -- the gateway answered 404 and re-provisioning
	// failed for anyone who already had a token.
	alias := KeyAlias(windowsUser)
	if _, found, err := gw.FindKeyByAlias(ctx, alias); err != nil {
		return fmt.Errorf("check existing token for %q: %w", windowsUser, err)
	} else if found {
		if err := gw.DeleteKeyByAlias(ctx, alias); err != nil {
			return fmt.Errorf("revoke previous token for %q: %w", windowsUser, err)
		}
	}

	key, err := gw.GenerateKey(ctx, alias, alias, allowed, map[string]string{"employee": windowsUser})
	if err != nil {
		if litellm.IsAliasTaken(err) {
			// Revoked a moment ago and taken again already: something else
			// provisioned this person between our revoke and our mint. The
			// lock above prevents that within this process, so this is
			// another console instance or an operator on the gateway UI.
			return fmt.Errorf("token for %q was just issued by another operation; refresh the page instead of retrying", windowsUser)
		}
		return fmt.Errorf("issue token for %q: %w", windowsUser, err)
	}

	catalogJSON, err := catalog.Build(available, allowed)
	if err != nil {
		// The token exists but nothing was delivered. Withdraw it rather
		// than leave an unreferenced token live on the gateway.
		_ = gw.DeleteKey(ctx, key.Key)
		return fmt.Errorf("build catalog for %q: %w", windowsUser, err)
	}

	configTOML := renderCodexConfig(cfg, windowsUser, allowed, key.Key)

	set := model.CredentialSet{
		model.PathCodexConfig: []byte(configTOML),
		model.PathCodexModels: catalogJSON,
	}
	if err := m.PublishCredentials(windowsUser, set); err != nil {
		_ = gw.DeleteKey(ctx, key.Key)
		return fmt.Errorf("deliver gateway configuration for %q: %w", windowsUser, err)
	}
	return nil
}

// setModelsLocked changes which models an employee may use and refreshes
// their catalog to match, without a lock of its own -- the caller
// (SetModels) holds lockProvision for windowsUser. It returns the resolved
// allowlist so the caller can audit exactly what was applied.
//
// Both the token and the catalog are required. The token governs what an
// employee *can* reach; the catalog governs what they can *see*. Updating
// only the token leaves withdrawn models listed in the picker; updating
// only the catalog leaves them reachable by typing the name.
func (m *Manager) setModelsLocked(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) ([]string, error) {
	us, err := m.LoadUsers()
	if err != nil {
		return nil, err
	}
	if us.Find(windowsUser) == nil {
		return nil, fmt.Errorf("user %q not found in roster", windowsUser)
	}

	alias := KeyAlias(windowsUser)
	key, found, err := gw.FindKeyByAlias(ctx, alias)
	if err != nil {
		return nil, fmt.Errorf("look up token for %q: %w", windowsUser, err)
	}
	if !found {
		return nil, fmt.Errorf("user %q has no gateway token; provision one first", windowsUser)
	}

	available, err := gw.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("read gateway model catalog: %w", err)
	}
	allowed, err := resolveAllowlist(available, models)
	if err != nil {
		return nil, err
	}
	if err := gw.UpdateKey(ctx, key.Handle(), allowed); err != nil {
		return nil, fmt.Errorf("update token for %q: %w", windowsUser, err)
	}

	if existingUser, found, err := gw.UserInfo(ctx, alias); err != nil {
		return nil, fmt.Errorf("look up gateway user %q: %w", alias, err)
	} else if found {
		q := existingUser.Quota()
		if err := m.ensureGatewayUser(ctx, gw, *us.Find(windowsUser), &q, allowed); err != nil {
			return nil, err
		}
	}

	catalogJSON, err := catalog.Build(available, allowed)
	if err != nil {
		return nil, fmt.Errorf("build catalog for %q: %w", windowsUser, err)
	}
	if err := m.PublishCredentials(windowsUser, model.CredentialSet{model.PathCodexModels: catalogJSON}); err != nil {
		return nil, err
	}
	return allowed, nil
}

// resolveAllowlist validates the requested models against what the gateway
// actually serves.
//
// Requesting a model the gateway does not have is rejected rather than
// silently dropped: it is almost always a typo in a slug, and a silent drop
// would show up much later as "why can't they use that model".
func resolveAllowlist(available []litellm.Model, requested []string) ([]string, error) {
	if len(available) == 0 {
		return nil, fmt.Errorf("gateway reports no catalog-visible models")
	}
	names := make(map[string]bool, len(available))
	for _, m := range available {
		names[m.Name] = true
	}

	if len(requested) == 0 {
		all := make([]string, 0, len(available))
		for _, m := range available {
			all = append(all, m.Name)
		}
		sort.Strings(all)
		return all, nil
	}

	var unknown []string
	allowed := make([]string, 0, len(requested))
	for _, r := range requested {
		if !names[r] {
			unknown = append(unknown, r)
			continue
		}
		allowed = append(allowed, r)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("gateway has no model named %s", strings.Join(unknown, ", "))
	}
	sort.Strings(allowed)
	return allowed, nil
}

// renderCodexConfig produces the config.toml fragment.
//
// Only the keys this tool owns appear here; the agent merges the fragment
// into whatever the employee already has rather than replacing the file.
func renderCodexConfig(cfg GatewayConfig, windowsUser string, allowed []string, token string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "model              = %q\n", allowed[0])
	fmt.Fprintf(&b, "model_provider     = \"gateway\"\n")
	// Windows paths are written with forward slashes in TOML.
	fmt.Fprintf(&b, "model_catalog_json = \"C:/Users/%s/.codex/models.json\"\n", windowsUser)
	fmt.Fprintf(&b, "web_search         = \"live\"\n")
	// Codex agent runs go for many minutes without emitting a token; the
	// default idle timeout would cut them off mid-task.
	fmt.Fprintf(&b, "stream_idle_timeout_ms = 7200000\n\n")
	fmt.Fprintf(&b, "[model_providers.gateway]\n")
	fmt.Fprintf(&b, "name     = \"Gateway\"\n")
	fmt.Fprintf(&b, "base_url = %q\n", strings.TrimRight(cfg.BaseURL, "/")+"/v1")
	fmt.Fprintf(&b, "wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "experimental_bearer_token = %q\n", token)
	return b.String()
}
