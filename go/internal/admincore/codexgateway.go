package admincore

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/catalog"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// Gateway is the subset of the LiteLLM client this package needs. Taking it
// as an interface keeps the onboarding logic testable without a live
// gateway, matching how Store is handled for the object store.
type Gateway interface {
	GenerateKey(ctx context.Context, alias string, models []string, maxBudget float64, metadata map[string]string) (litellm.Key, error)
	UpdateKey(ctx context.Context, key string, models []string) error
	DeleteKey(ctx context.Context, keys ...string) error
	FindKeyByAlias(ctx context.Context, alias string) (litellm.Key, bool, error)
	Models(ctx context.Context) ([]litellm.Model, error)
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

// ProvisionCodexGateway issues a gateway token for one employee and delivers
// the Codex configuration that uses it.
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
func (m *Manager) ProvisionCodexGateway(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string, maxBudget float64) error {
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

	available, err := gw.Models(ctx)
	if err != nil {
		return fmt.Errorf("read gateway model catalog: %w", err)
	}
	allowed, err := resolveAllowlist(available, models)
	if err != nil {
		return err
	}

	alias := KeyAlias(windowsUser)
	if existing, found, err := gw.FindKeyByAlias(ctx, alias); err != nil {
		return fmt.Errorf("check existing token for %q: %w", windowsUser, err)
	} else if found {
		if err := gw.DeleteKey(ctx, existing.Key); err != nil {
			return fmt.Errorf("revoke previous token for %q: %w", windowsUser, err)
		}
	}

	key, err := gw.GenerateKey(ctx, alias, allowed, maxBudget, map[string]string{"employee": windowsUser})
	if err != nil {
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

// SetCodexGatewayModels changes which models an employee may use and
// refreshes their catalog to match.
//
// Both halves are required. The token governs what they *can* reach; the
// catalog governs what they can *see*. Updating only the token leaves
// withdrawn models listed in the picker; updating only the catalog leaves
// them reachable by typing the name.
func (m *Manager) SetCodexGatewayModels(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) error {
	alias := KeyAlias(windowsUser)
	key, found, err := gw.FindKeyByAlias(ctx, alias)
	if err != nil {
		return fmt.Errorf("look up token for %q: %w", windowsUser, err)
	}
	if !found {
		return fmt.Errorf("user %q has no gateway token; provision one first", windowsUser)
	}

	available, err := gw.Models(ctx)
	if err != nil {
		return fmt.Errorf("read gateway model catalog: %w", err)
	}
	allowed, err := resolveAllowlist(available, models)
	if err != nil {
		return err
	}
	if err := gw.UpdateKey(ctx, key.Key, allowed); err != nil {
		return fmt.Errorf("update token for %q: %w", windowsUser, err)
	}

	catalogJSON, err := catalog.Build(available, allowed)
	if err != nil {
		return fmt.Errorf("build catalog for %q: %w", windowsUser, err)
	}
	return m.PublishCredentials(windowsUser, model.CredentialSet{model.PathCodexModels: catalogJSON})
}

// RevokeCodexGateway withdraws an employee's gateway token.
//
// This is the half that does not depend on the machine being reachable, and
// so it is the half that actually enforces offboarding. Clearing the local
// files is the agent's job, driven by the object being deleted from the
// store; the two are independent on purpose.
//
// A user with no token is not an error: offboarding runs against everyone
// being removed, including those who never had Codex provisioned.
func (m *Manager) RevokeCodexGateway(ctx context.Context, gw Gateway, windowsUser string) error {
	key, found, err := gw.FindKeyByAlias(ctx, KeyAlias(windowsUser))
	if err != nil {
		return fmt.Errorf("look up token for %q: %w", windowsUser, err)
	}
	if !found {
		return nil
	}
	if err := gw.DeleteKey(ctx, key.Key); err != nil {
		return fmt.Errorf("revoke token for %q: %w", windowsUser, err)
	}
	return nil
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
