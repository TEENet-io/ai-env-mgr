package admincore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// fakeGateway records what the orchestration asked the gateway to do.
type fakeGateway struct {
	mu     *sync.Mutex // nil in single-threaded tests
	models []litellm.Model

	existing  map[string]litellm.Key
	generated []litellm.Key
	deleted   []string
	updated   map[string][]string

	users   map[string]litellm.User
	upserts []litellm.UserSpec

	generateErr    error
	modelsErr      error
	upsertErr      error
	listKeysErr    error
	deleteAliasErr error
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		models: []litellm.Model{
			{Name: "grok-4.6", Info: litellm.ModelInfo{DisplayName: "Grok 4.6", ContextWindow: 256000, CatalogVisible: true}},
			{Name: "glm-5", Info: litellm.ModelInfo{DisplayName: "GLM-5", ContextWindow: 128000, CatalogVisible: true}},
		},
		existing: map[string]litellm.Key{},
		updated:  map[string][]string{},
		users:    map[string]litellm.User{},
	}
}

func (f *fakeGateway) lock() func() {
	if f.mu == nil {
		return func() {}
	}
	f.mu.Lock()
	return f.mu.Unlock
}

func (f *fakeGateway) GenerateKey(_ context.Context, alias, userID string, models []string, _ map[string]string) (litellm.Key, error) {
	defer f.lock()()
	if f.generateErr != nil {
		return litellm.Key{}, f.generateErr
	}
	if userID == "" {
		return litellm.Key{}, fmt.Errorf("fake gateway: key without user_id")
	}
	if _, taken := f.existing[alias]; taken {
		return litellm.Key{}, &litellm.APIError{Status: 400, Path: "/key/generate", Body: "Key with alias '" + alias + "' already exists."}
	}
	k := litellm.Key{Key: "sk-" + alias, KeyAlias: alias, UserID: userID, Models: models}
	f.generated = append(f.generated, k)
	// What the gateway later lists: the hash, never the plaintext. This is
	// the detail a first version of the fake got wrong, which let code that
	// revoked by plaintext pass every test and fail against the real thing.
	f.existing[alias] = litellm.Key{Token: "hash-of-" + alias, KeyAlias: alias, UserID: userID, Models: models}
	return k, nil
}

func (f *fakeGateway) UpdateKey(_ context.Context, key string, models []string) error {
	f.updated[key] = models
	return nil
}

func (f *fakeGateway) DeleteKey(_ context.Context, handles ...string) error {
	for _, h := range handles {
		if h == "" {
			// The real gateway answers an empty key with 404 "No keys found".
			return fmt.Errorf("gateway /key/delete returned 404: No keys found")
		}
	}
	f.deleted = append(f.deleted, handles...)
	for alias, k := range f.existing {
		for _, target := range handles {
			if k.Key == target || k.Token == target {
				delete(f.existing, alias)
			}
		}
	}
	return nil
}

func (f *fakeGateway) DeleteKeyByAlias(_ context.Context, alias string) error {
	defer f.lock()()
	if f.deleteAliasErr != nil {
		return f.deleteAliasErr
	}
	k, ok := f.existing[alias]
	if !ok {
		return fmt.Errorf("gateway /key/delete returned 404: No keys found")
	}
	f.deleted = append(f.deleted, "alias:"+alias)
	_ = k
	delete(f.existing, alias)
	return nil
}

func (f *fakeGateway) FindKeyByAlias(_ context.Context, alias string) (litellm.Key, bool, error) {
	defer f.lock()()
	k, ok := f.existing[alias]
	return k, ok, nil
}

func (f *fakeGateway) Models(context.Context) ([]litellm.Model, error) {
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return f.models, nil
}

func (f *fakeGateway) ListKeys(context.Context) ([]litellm.Key, error) {
	defer f.lock()()
	if f.listKeysErr != nil {
		return nil, f.listKeysErr
	}
	out := make([]litellm.Key, 0, len(f.existing))
	for _, k := range f.existing {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeGateway) UpsertUser(_ context.Context, spec litellm.UserSpec) error {
	defer f.lock()()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	if err := spec.Quota.Validate(); err != nil {
		return err
	}
	f.upserts = append(f.upserts, spec)
	u := f.users[spec.UserID] // keep spend across updates, as the gateway does
	budget, rpm, tpm, par := spec.Quota.MonthlyBudgetUSD, spec.Quota.RPM, spec.Quota.TPM, spec.Quota.Parallel
	u.UserID, u.Alias = spec.UserID, spec.Alias
	u.MaxBudget, u.RPMLimit, u.TPMLimit, u.MaxParallel = &budget, &rpm, &tpm, &par
	u.BudgetDuration, u.BudgetResetAt = "1mo", "2026-10-01T00:00:00Z"
	u.Models = spec.Models
	u.Metadata = map[string]string{"department": spec.Department}
	f.users[spec.UserID] = u
	return nil
}

func (f *fakeGateway) UserInfo(_ context.Context, userID string) (litellm.User, bool, error) {
	defer f.lock()()
	u, ok := f.users[userID]
	return u, ok, nil
}

func (f *fakeGateway) ListUsers(context.Context) ([]litellm.User, error) {
	defer f.lock()()
	out := make([]litellm.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	return out, nil
}

// managerWithUser returns a manager whose roster already contains user, in
// service: every provisioning path requires an entry before it will issue
// anything, and refuses one that has been offboarded.
func managerWithUser(t *testing.T, user string) (*Manager, *fakeStore) {
	t.Helper()
	m, store := newManager()
	if err := m.SaveUsers(model.Users{Users: []model.UserEntry{{WindowsUser: user, Enabled: true}}}); err != nil {
		t.Fatalf("seed roster: %v", err)
	}
	return m, store
}

func deliveredSet(t *testing.T, store *fakeStore, user string) model.CredentialSet {
	t.Helper()
	blob, ok := store.objects[ossclient.UserKey(user, "credentials.zip")]
	if !ok {
		t.Fatal("nothing was delivered to the object store")
	}
	set, err := creds.Unpack(blob)
	if err != nil {
		t.Fatalf("unpack delivered archive: %v", err)
	}
	return set
}

func TestProvisionDeliversConfigAndCatalog(t *testing.T) {
	m, store := managerWithUser(t, "alice")
	gw := newFakeGateway()

	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}

	set := deliveredSet(t, store, "alice")
	cfg := string(set[model.PathCodexConfig])
	if !strings.Contains(cfg, `base_url = "https://gw.example/v1"`) {
		t.Errorf("config does not point at the gateway:\n%s", cfg)
	}
	if !strings.Contains(cfg, `wire_api = "responses"`) {
		t.Errorf("wire_api must be responses; Codex speaks nothing else:\n%s", cfg)
	}
	if !strings.Contains(cfg, "sk-emp-alice") {
		t.Errorf("token not delivered:\n%s", cfg)
	}
	if !strings.Contains(cfg, "stream_idle_timeout_ms = 7200000") {
		t.Errorf("long-task timeout missing; agent runs would be cut off:\n%s", cfg)
	}

	var catalogDoc struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(set[model.PathCodexModels], &catalogDoc); err != nil {
		t.Fatalf("catalog is not valid JSON: %v", err)
	}
	if len(catalogDoc.Models) != 2 {
		t.Errorf("expected both gateway models in the catalog, got %d", len(catalogDoc.Models))
	}
}

func TestProvisionRevokesPreviousTokenForSameEmployee(t *testing.T) {
	// A second live token for one person cannot be attributed or reconciled.
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()
	// As the real gateway lists it: hash only, no plaintext.
	gw.existing["emp-alice"] = litellm.Key{Token: "hash-old", KeyAlias: "emp-alice"}

	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != "alias:emp-alice" {
		t.Errorf("previous token must be revoked by alias, deleted = %v", gw.deleted)
	}
}

func TestProvisionWithdrawsTokenWhenDeliveryFails(t *testing.T) {
	// Otherwise the gateway accumulates live tokens nobody holds.
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()
	gw.models = nil // forces catalog.Build to fail after the token is minted
	gw.modelsErr = nil

	err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil)
	if err == nil {
		t.Fatal("expected provisioning to fail with no models")
	}
	if len(gw.generated) > 0 && len(gw.deleted) == 0 {
		t.Error("a token was issued but not withdrawn after the failure")
	}
}

func TestProvisionRejectsUnknownModel(t *testing.T) {
	// A typo in a slug must surface now, not as "why can't they use that".
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()

	err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", []string{"grok-4.6", "gpt-9"})
	if err == nil || !strings.Contains(err.Error(), "gpt-9") {
		t.Fatalf("expected the unknown slug to be named, got %v", err)
	}
	if len(gw.generated) != 0 {
		t.Error("a token was issued despite an invalid request")
	}
}

func TestProvisionRejectsUserNotOnRoster(t *testing.T) {
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()
	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "mallory", nil); err == nil {
		t.Fatal("expected provisioning an unknown user to fail")
	}
	if len(gw.generated) != 0 {
		t.Error("a token was issued for someone not on the roster")
	}
}

func TestCatalogIsRestrictedToTheEmployeeAllowlist(t *testing.T) {
	m, store := managerWithUser(t, "alice")
	gw := newFakeGateway()

	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", []string{"glm-5"}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	set := deliveredSet(t, store, "alice")

	var doc struct {
		Models []map[string]any `json:"models"`
	}
	_ = json.Unmarshal(set[model.PathCodexModels], &doc)
	if len(doc.Models) != 1 || doc.Models[0]["slug"] != "glm-5" {
		t.Fatalf("picker would offer models the gateway refuses: %v", doc.Models)
	}
	if def := string(set[model.PathCodexConfig]); !strings.Contains(def, `model              = "glm-5"`) {
		t.Errorf("default model must be one the employee may use:\n%s", def)
	}
}

func TestSetModelsUpdatesTokenAndCatalogTogether(t *testing.T) {
	m, store := managerWithUser(t, "alice")
	gw := newFakeGateway()
	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}

	if err := m.SetModels(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", []string{"glm-5"}); err != nil {
		t.Fatalf("set models: %v", err)
	}

	// The token was found via the listing, so it is updated by its hash.
	if got := gw.updated["hash-of-emp-alice"]; len(got) != 1 || got[0] != "glm-5" {
		t.Errorf("token allowlist not narrowed: %v", got)
	}
	set := deliveredSet(t, store, "alice")
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	_ = json.Unmarshal(set[model.PathCodexModels], &doc)
	if len(doc.Models) != 1 {
		t.Errorf("catalog still lists withdrawn models: %v", doc.Models)
	}
	// The previously delivered config must survive a catalog-only update.
	if _, ok := set[model.PathCodexConfig]; !ok {
		t.Error("updating the catalog dropped the delivered config.toml")
	}
}

func TestKeyAliasIsDeterministicAndCaseInsensitive(t *testing.T) {
	if KeyAlias("Alice") != KeyAlias("alice") {
		t.Error("alias must not vary with the casing of the Windows user name")
	}
}

func TestConcurrentProvisionsForOneEmployeeDoNotCollide(t *testing.T) {
	// A double-click used to make the second request find no token (the
	// first had just revoked it), skip revocation, and collide with the
	// token the first was minting. Serialised, both must succeed and leave
	// exactly one live token.
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()
	gw.mu = &sync.Mutex{}
	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil); err != nil {
		t.Fatalf("initial provision: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent provision failed: %v", err)
		}
	}
	if n := len(gw.existing); n != 1 {
		t.Errorf("expected exactly one live token, found %d", n)
	}
}

func TestProvisionCreatesGatewayUserWhenMissing(t *testing.T) {
	// A key needs an owner or the budget has nothing to hang on. Existing
	// employees (weipeng, 2026-09) have a key and no user; re-issuing must
	// heal that rather than fail.
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()

	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	u, ok := gw.users["emp-alice"]
	if !ok {
		t.Fatal("gateway user was not created")
	}
	if u.Quota() != DefaultQuota {
		t.Errorf("a user created on the fly gets the built-in defaults, got %+v", u.Quota())
	}
	if len(gw.generated) != 1 || gw.generated[0].UserID != "emp-alice" {
		t.Errorf("key not minted under the user: %+v", gw.generated)
	}
}

func TestProvisionKeepsExistingUserQuota(t *testing.T) {
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()
	budget := 55.0
	gw.users["emp-alice"] = litellm.User{UserID: "emp-alice", MaxBudget: &budget}

	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(gw.upserts) != 0 {
		t.Errorf("re-issuing a token must not rewrite the user's quota: %+v", gw.upserts)
	}
}

func TestReissueKeepsNarrowedAllowlist(t *testing.T) {
	m, store, gw := onboarded(t) // alice: glm-5 only
	if err := m.Reissue(context.Background(), gw, testGW, "alice"); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if got := gw.generated[1].Models; len(got) != 1 || got[0] != "glm-5" {
		t.Errorf("reissue widened the allowlist: %v", got)
	}
	if strings.Contains(string(deliveredSet(t, store, "alice")[model.PathCodexModels]), `"slug": "grok-4.6"`) {
		t.Error("catalog widened on reissue")
	}
}
