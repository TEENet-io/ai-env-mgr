package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

// fakeGateway stands in for LiteLLM: it remembers users and keys, and can be
// told to fail in the ways the real one does.
type fakeGateway struct {
	mu       sync.Mutex
	users    map[string]litellm.UserSpec
	keys     map[string]litellm.Key
	minted   int
	calls    []string
	failWith map[string]error
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		users: map[string]litellm.UserSpec{}, keys: map[string]litellm.Key{},
		failWith: map[string]error{},
	}
}

func (g *fakeGateway) record(call string) error {
	g.calls = append(g.calls, call)
	if err, ok := g.failWith[call]; ok {
		return err
	}
	return nil
}

func (g *fakeGateway) UpsertUser(_ context.Context, spec litellm.UserSpec) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("UpsertUser"); err != nil {
		return err
	}
	g.users[spec.UserID] = spec
	return nil
}

func (g *fakeGateway) GenerateKey(_ context.Context, alias, userID string, models []string, _ map[string]string) (litellm.Key, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("GenerateKey"); err != nil {
		return litellm.Key{}, err
	}
	if _, taken := g.keys[alias]; taken {
		return litellm.Key{}, &litellm.APIError{Status: 400, Path: "/key/generate", Body: "alias already exists"}
	}
	g.minted++
	key := litellm.Key{
		Key: fmt.Sprintf("sk-fake-%d", g.minted), Token: fmt.Sprintf("hash-%d", g.minted),
		KeyAlias: alias, UserID: userID, Models: models,
	}
	g.keys[alias] = key
	return key, nil
}

func (g *fakeGateway) FindKeyByAlias(_ context.Context, alias string) (litellm.Key, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("FindKeyByAlias"); err != nil {
		return litellm.Key{}, false, err
	}
	key, ok := g.keys[alias]
	// The gateway never returns the plaintext again; only the hash.
	key.Key = ""
	return key, ok, nil
}

func (g *fakeGateway) DeleteKeyByAlias(_ context.Context, alias string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("DeleteKeyByAlias"); err != nil {
		return err
	}
	delete(g.keys, alias)
	return nil
}

func (g *fakeGateway) DeleteUser(_ context.Context, userID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("DeleteUser"); err != nil {
		return err
	}
	delete(g.users, userID)
	return nil
}

func (g *fakeGateway) UpdateKey(_ context.Context, handle string, models []string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.record("UpdateKey"); err != nil {
		return err
	}
	for alias, key := range g.keys {
		if key.Key == handle || key.Token == handle {
			key.Models = models
			g.keys[alias] = key
			return nil
		}
	}
	return &litellm.APIError{Status: 404, Path: "/key/update", Body: "no such key"}
}

func (g *fakeGateway) aliases() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []string{}
	for alias := range g.keys {
		out = append(out, alias)
	}
	return out
}

func testKeyring(t *testing.T) secrets.Keyring {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	body := `{"current":"k1","keys":{"k1":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ring, err := secrets.NewFileKeyring(path)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}

// provisioned sets up an onboarded employee and a worker wired to a fake
// gateway, and returns everything the tests poke at.
func provisioned(t *testing.T) (*dbstore.Store, *ops.Service, *fakeGateway, secrets.Keyring, *Worker, context.Context) {
	t.Helper()
	store, ctx := newWorkerStore(t)
	service := ops.New(store)
	gateway := newFakeGateway()
	ring := testKeyring(t)

	// A short backoff so a retry test does not have to wait out the real one.
	w := New(store, Options{Owner: "worker-1", BaseBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	w.Register(repo.TaskGatewayProvision, GatewayProvision{Store: store, Gateway: gateway, Keyring: ring})
	w.Register(repo.TaskGatewayRevoke, GatewayRevoke{Store: store, Gateway: gateway})
	// The export is somebody else's job; a no-op keeps it out of the way.
	w.Register(repo.TaskOSSExport, HandlerFunc(func(context.Context, repo.Task) (Result, error) {
		return Result{}, nil
	}))
	return store, service, gateway, ring, w, ctx
}

func drain(t *testing.T, ctx context.Context, w *Worker) {
	t.Helper()
	for range 20 {
		worked, err := w.RunOnce(ctx)
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("the queue did not empty")
}

func onboard(t *testing.T, ctx context.Context, service *ops.Service, user string) repo.Employee {
	t.Helper()
	employee, err := service.Onboard(ctx, ops.OnboardSpec{
		WindowsUser: user, Name: "张三", Department: "研发",
		Quota:  repo.Quota{MonthlyBudget: "50", RPM: 60, TPM: 2000000, Parallel: 8},
		Models: []string{"claude-4.5-sonnet"},
		Actor:  "zhang",
	})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	return employee
}

func TestProvisioningIssuesOneTokenAndStoresItSealed(t *testing.T) {
	store, service, gateway, ring, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)

	// The limits go on the user, not the key, so that re-issuing a token does
	// not reset the month's spend to zero.
	spec, ok := gateway.users[GatewayUserID("work1")]
	if !ok {
		t.Fatalf("no gateway user was created; users = %v", gateway.users)
	}
	if spec.Quota.MonthlyBudgetUSD != 50 || spec.Quota.TPM != 2000000 || spec.Alias != "张三" {
		t.Errorf("gateway user = %+v", spec)
	}

	alias := KeyAlias("work1", employee.ID, employee.AuthEpoch)
	if got := gateway.aliases(); len(got) != 1 || got[0] != alias {
		t.Fatalf("gateway holds %v, want just %s", got, alias)
	}

	grant, err := store.Grants().Active(ctx, "", employee.ID)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.KeyAlias != alias || grant.Actual != repo.ActualActive {
		t.Errorf("grant = %+v, want the alias observed active", grant)
	}

	credential, err := store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if credential.ID != grant.CredentialID {
		t.Error("the grant does not point at the credential that was stored")
	}
	// The token is in the database only as ciphertext.
	if bytes.Contains(credential.Ciphertext, []byte("sk-fake")) {
		t.Fatal("the token is stored in the clear")
	}
	plain, err := ring.Open(ctx, credential.Ciphertext, credential.KeyVersion,
		secrets.AAD("credential_versions", employee.ID, repo.PurposeCodexGateway))
	if err != nil {
		t.Fatalf("open the stored token: %v", err)
	}
	if !strings.HasPrefix(string(plain), "sk-fake") {
		t.Errorf("stored token = %q", plain)
	}
}

func TestProvisioningTwiceMintsOneToken(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)
	minted := gateway.minted

	// The same task again -- which is what at-least-once delivery means --
	// must not produce a second live token for one person.
	task, _, err := store.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           repo.TaskGatewayProvision,
		IdempotencyKey: "replay",
		Payload: []byte(fmt.Sprintf(`{"employee_id":%q,"windows_user":"work1","epoch":%d}`,
			employee.ID, employee.AuthEpoch)),
		TargetEpoch: &employee.AuthEpoch, EmployeeID: employee.ID,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, ctx, w)

	if gateway.minted != minted {
		t.Errorf("a replay minted another token: %d then %d", minted, gateway.minted)
	}
	done, err := store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if done.Status != repo.TaskSucceeded {
		t.Errorf("the replayed task is %s, want succeeded", done.Status)
	}
	if got := gateway.aliases(); len(got) != 1 {
		t.Errorf("gateway holds %v, want one key", got)
	}
}

func TestReissuingReplacesTheTokenOnTheGateway(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)
	firstAlias := KeyAlias("work1", employee.ID, employee.AuthEpoch)

	reissued, err := service.Reissue(ctx, employee.ID, "zhang", "")
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	drain(t, ctx, w)

	// The old key is gone from the gateway, not merely marked revoked here. A
	// key the gateway still serves for a grant we consider revoked is a token
	// nobody thinks exists.
	secondAlias := KeyAlias("work1", reissued.ID, reissued.AuthEpoch)
	if got := gateway.aliases(); len(got) != 1 || got[0] != secondAlias {
		t.Fatalf("gateway holds %v, want only %s", got, secondAlias)
	}
	old, err := store.Grants().ByKeyAlias(ctx, "", firstAlias)
	if err != nil {
		t.Fatalf("old grant: %v", err)
	}
	if old.Desired != repo.GrantRevoked || old.Actual != repo.ActualRevoked {
		t.Errorf("old grant = %+v, want revoked and observed revoked", old)
	}
	// The gateway user survives: it owns the spend history, which must not
	// restart every time somebody's token is replaced.
	if _, ok := gateway.users[GatewayUserID("work1")]; !ok {
		t.Error("the gateway user was removed by a re-issue")
	}
}

func TestOffboardingRevokesTheTokenButKeepsTheUser(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)

	if _, err := service.Offboard(ctx, employee.ID, "zhang", ""); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	drain(t, ctx, w)

	if got := gateway.aliases(); len(got) != 0 {
		t.Fatalf("gateway still holds %v after offboarding", got)
	}
	// Deleting the gateway user would delete every key under it and take the
	// spend history with it -- verified against the live gateway 2026-09-18.
	if _, ok := gateway.users[GatewayUserID("work1")]; !ok {
		t.Error("offboarding deleted the gateway user")
	}
	grants, err := store.Grants().ByEmployee(ctx, employee.ID)
	if err != nil {
		t.Fatalf("grants: %v", err)
	}
	for _, grant := range grants {
		if grant.Actual != repo.ActualRevoked {
			t.Errorf("grant %s is %s, want revoked", grant.KeyAlias, grant.Actual)
		}
	}

	// Running the revoke again is what at-least-once delivery does.
	if _, _, err := store.Tasks().Enqueue(ctx, repo.NewTask{
		Kind: repo.TaskGatewayRevoke, IdempotencyKey: "replay-revoke",
		Payload: []byte(fmt.Sprintf(`{"employee_id":%q}`, employee.ID)),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, ctx, w)
}

func TestAGatewayThatIsDownDelaysRatherThanLoses(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	gateway.failWith["UpsertUser"] = &litellm.APIError{Status: 502, Path: "/user/new", Body: "bad gateway"}
	employee := onboard(t, ctx, service, "work1")

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	tasks, err := store.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var provision repo.Task
	for _, task := range tasks {
		if task.Kind == repo.TaskGatewayProvision {
			provision = task
		}
	}
	if provision.Status != repo.TaskRetryWait {
		t.Fatalf("the provisioning task is %s, want it waiting to be retried", provision.Status)
	}
	if _, err := store.Grants().Active(ctx, "", employee.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Error("a grant was recorded for a token the gateway never issued")
	}

	// The gateway comes back; the work resumes with no intervention.
	delete(gateway.failWith, "UpsertUser")
	if _, err := store.Tasks().Fail(ctx, provision.ID, "nobody", provision.NextRunAt, "", "", ""); err == nil {
		t.Fatal("a task that is not held was allowed to report a failure")
	}
	time.Sleep(30 * time.Millisecond) // let the backoff elapse
	drain(t, ctx, w)

	if _, err := store.Grants().Active(ctx, "", employee.ID); err != nil {
		t.Fatalf("after the gateway recovered, there is still no grant: %v", err)
	}
}

// A 4xx is the gateway saying the request itself is wrong; the tenth identical
// request will be wrong too.
func TestARejectedRequestIsNotRetriedForever(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	gateway.failWith["UpsertUser"] = &litellm.APIError{
		Status: 400, Path: "/user/new", Body: "models: unknown model"}
	onboard(t, ctx, service, "work1")

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	tasks, err := store.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, task := range tasks {
		if task.Kind == repo.TaskGatewayProvision {
			t.Fatalf("the rejected task is still open as %s", task.Status)
		}
	}
}

// A key minted by an attempt that then failed leaves a token we hold no
// plaintext for. It cannot be recovered -- /key/generate returns it once --
// so the only way back to a known state is to replace it.
func TestAnOrphanedKeyIsReplacedRatherThanAdopted(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")

	alias := KeyAlias("work1", employee.ID, employee.AuthEpoch)
	if _, err := gateway.GenerateKey(ctx, alias, GatewayUserID("work1"), nil, nil); err != nil {
		t.Fatalf("plant an orphaned key: %v", err)
	}
	drain(t, ctx, w)

	if got := gateway.aliases(); len(got) != 1 || got[0] != alias {
		t.Fatalf("gateway holds %v", got)
	}
	credential, err := store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	// The stored token must be the one we can actually open, not the orphan.
	if credential.ID == "" {
		t.Fatal("no credential was stored")
	}
	if gateway.minted < 2 {
		t.Error("the orphaned key was adopted instead of replaced")
	}
}

// Changing the models after a token exists has to change the token: on the
// gateway the key's own allowlist takes precedence over the user's, so pushing
// the user and leaving the key would let the old models through.
func TestChangingModelsUpdatesTheExistingToken(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)
	minted := gateway.minted

	if err := service.SetModels(ctx, employee.ID, []string{"gemini-2.5-pro"}, "zhang", ""); err != nil {
		t.Fatalf("set models: %v", err)
	}
	drain(t, ctx, w)

	alias := KeyAlias("work1", employee.ID, employee.AuthEpoch)
	gateway.mu.Lock()
	key := gateway.keys[alias]
	gateway.mu.Unlock()
	if len(key.Models) != 1 || key.Models[0] != "gemini-2.5-pro" {
		t.Errorf("the token still allows %v", key.Models)
	}
	if gateway.minted != minted {
		t.Error("a model change minted a new token instead of updating the existing one")
	}
	grant, err := store.Grants().Active(ctx, "", employee.ID)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if len(grant.Models) != 1 || grant.Models[0] != "gemini-2.5-pro" {
		t.Errorf("the grant records %v", grant.Models)
	}
}

func TestDeletingAnAccountRemovesTheGatewayUserAndItsTokens(t *testing.T) {
	store, ctx := newWorkerStore(t)
	gw := newFakeGateway()
	e, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	if err != nil {
		t.Fatal(err)
	}
	gw.users["emp-work1"] = litellm.UserSpec{UserID: "emp-work1"}
	// A token the offboarding never got to revoke.
	cred, _ := store.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: e.ID, Epoch: 1, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	grant, err := store.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: e.ID, Epoch: 1, ExternalUser: "emp-work1", KeyAlias: "emp-work1-e1", CredentialID: cred.ID})
	if err != nil {
		t.Fatal(err)
	}
	gw.keys["emp-work1-e1"] = litellm.Key{KeyAlias: "emp-work1-e1"}

	h := GatewayDelete{Store: store, Gateway: gw}
	task := repo.Task{Payload: mustJSON(t, map[string]any{"employee_id": e.ID, "windows_user": "work1"})}
	if _, err := h.Run(ctx, task); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, still := gw.users["emp-work1"]; still {
		t.Fatal("the gateway user is still there")
	}
	if _, still := gw.keys["emp-work1-e1"]; still {
		t.Fatal("the token outlived its user")
	}
	if got, _ := store.Grants().ByEmployee(ctx, e.ID); len(got) != 1 || got[0].Actual != repo.ActualRevoked || got[0].ID != grant.ID {
		t.Fatalf("grant after delete = %+v", got)
	}
	// Running again finds nothing to do and is not an error.
	if _, err := h.Run(ctx, task); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestProvisioningQueuesTheDeliveryItMadePossible(t *testing.T) {
	store, ctx := newWorkerStore(t)
	gw := newFakeGateway()
	ring := testKeyring(t)
	service := ops.New(store)
	employee := onboard(t, ctx, service, "work1")
	exportsFor := func() []repo.Task {
		var out []repo.Task
		open, _ := store.Tasks().ListOpen(ctx, 50)
		for _, task := range open {
			if task.Kind == repo.TaskOSSExport && task.EmployeeID == employee.ID {
				out = append(out, task)
			}
		}
		return out
	}
	// The export queued by onboarding ran first and found no token to
	// deliver: it finished with nothing on the machine.
	for range exportsFor() {
		c, err := store.Tasks().Claim(ctx, "w", []string{repo.TaskOSSExport}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		store.Tasks().Succeed(ctx, c.ID, "w", "")
	}
	if len(exportsFor()) != 0 {
		t.Fatal("setup: exports still open")
	}

	provision, err := store.Tasks().Claim(ctx, "w", []string{repo.TaskGatewayProvision}, time.Minute)
	if err != nil {
		t.Fatalf("claim provision: %v", err)
	}
	h := GatewayProvision{Store: store, Gateway: gw, Keyring: ring}
	if _, err := h.Run(ctx, provision); err != nil {
		t.Fatalf("provision: %v", err)
	}
	exports := exportsFor()
	if len(exports) != 1 || !strings.Contains(exports[0].IdempotencyKey, ":issued:") {
		t.Fatalf("after provisioning there must be a fresh export for the employee, got %+v", exports)
	}
}

func TestAReusedNameStartsItsAliasesAfresh(t *testing.T) {
	a := KeyAlias("work1", "0e5b2f1c-aaaa-bbbb-cccc-000000000001", 1)
	b := KeyAlias("work1", "9d4c7a20-aaaa-bbbb-cccc-000000000002", 1)
	if a == b || !strings.HasPrefix(a, "emp-work1-0e5b2f1c-e1") {
		t.Fatalf("aliases %q and %q", a, b)
	}
}

// An account provisioned before aliases carried the employee id holds a grant
// under the old name. The handler must find that grant by employee and epoch
// and keep using its alias, or every change to the account mints a second
// token and the one-active-grant rule refuses it.
func TestAnOldStyleAliasIsReusedNotReplaced(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	oldAlias := "emp-work1-e" + strconv.Itoa(employee.AuthEpoch)
	cred, err := store.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: employee.ID, Epoch: employee.AuthEpoch, Purpose: repo.PurposeCodexGateway,
		Ciphertext: []byte("sealed"), KeyVersion: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := store.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: employee.ID, Epoch: employee.AuthEpoch, ExternalUser: "emp-work1",
		KeyAlias: oldAlias, Models: []string{"claude-4.5-sonnet"}, CredentialID: cred.ID})
	if err != nil {
		t.Fatal(err)
	}
	store.Grants().RecordActual(ctx, grant.ID, repo.ActualActive, "")
	gateway.users["emp-work1"] = litellm.UserSpec{UserID: "emp-work1"}
	gateway.keys[oldAlias] = litellm.Key{KeyAlias: oldAlias, Token: "hash-old", UserID: "emp-work1", Models: []string{"claude-4.5-sonnet"}}

	// The provision queued by onboarding runs against the existing token.
	drain(t, ctx, w)
	if gateway.minted != 0 {
		t.Fatalf("minted %d new token(s) for an account that already has one", gateway.minted)
	}
	if err := service.SetModels(ctx, employee.ID, []string{"gemini-2.5-pro"}, "zhang", ""); err != nil {
		t.Fatalf("set models: %v", err)
	}
	drain(t, ctx, w)
	if gateway.minted != 0 {
		t.Fatalf("a model change minted %d token(s) instead of updating %s", gateway.minted, oldAlias)
	}
	gateway.mu.Lock()
	key := gateway.keys[oldAlias]
	gateway.mu.Unlock()
	if len(key.Models) != 1 || key.Models[0] != "gemini-2.5-pro" {
		t.Errorf("the old token still allows %v", key.Models)
	}
	active, err := store.Grants().Active(ctx, "", employee.ID)
	if err != nil || active.KeyAlias != oldAlias || len(active.Models) != 1 || active.Models[0] != "gemini-2.5-pro" {
		t.Errorf("active grant = %+v, %v", active, err)
	}
	open, _ := store.Tasks().ListOpen(ctx, 50)
	for _, task := range open {
		if task.Kind == repo.TaskGatewayProvision {
			t.Errorf("a provision task is still open: %+v", task)
		}
	}
}

// The gateway user is named after the Windows user, so when a deleted
// account's name is reused, the old account's clean-up must not take the
// new account's user with it.
func TestDeletingAnOldAccountSparesItsNamesake(t *testing.T) {
	store, ctx := newWorkerStore(t)
	gw := newFakeGateway()
	old, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := store.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: old.ID, Epoch: 1, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	if _, err := store.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: old.ID, Epoch: 1, ExternalUser: "emp-work1", KeyAlias: "emp-work1-e1", CredentialID: cred.ID}); err != nil {
		t.Fatal(err)
	}
	if old, err = store.Employees().Offboard(ctx, old.ID, old.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Employees().Delete(ctx, old.ID, old.Version); err != nil {
		t.Fatal(err)
	}
	// The gateway was down for the delete; meanwhile the name was reused
	// and the new account provisioned.
	fresh, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	if err != nil {
		t.Fatalf("reuse the name: %v", err)
	}
	newAlias := KeyAlias("work1", fresh.ID, 1)
	gw.users["emp-work1"] = litellm.UserSpec{UserID: "emp-work1", Alias: "new"}
	gw.keys["emp-work1-e1"] = litellm.Key{KeyAlias: "emp-work1-e1"}
	gw.keys[newAlias] = litellm.Key{KeyAlias: newAlias}

	h := GatewayDelete{Store: store, Gateway: gw}
	task := repo.Task{Payload: mustJSON(t, map[string]any{"employee_id": old.ID, "windows_user": "work1"})}
	if _, err := h.Run(ctx, task); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, still := gw.keys["emp-work1-e1"]; still {
		t.Error("the old account's token is still on the gateway")
	}
	if _, kept := gw.keys[newAlias]; !kept {
		t.Error("the new account's token was deleted")
	}
	if _, kept := gw.users["emp-work1"]; !kept {
		t.Error("the gateway user now belongs to the new account and must stay")
	}
}

// outerTasksFail is a store whose task queue works inside transactions and
// fails outside them. It catches a handler that stores a credential in one
// transaction and queues its delivery in another.
type outerTasksFail struct{ *dbstore.Store }

func (s outerTasksFail) Tasks() repo.Tasks { return failingTasks{s.Store.Tasks()} }

type failingTasks struct{ repo.Tasks }

func (failingTasks) Enqueue(context.Context, repo.NewTask) (repo.Task, bool, error) {
	return repo.Task{}, false, errors.New("queue unavailable")
}

func TestTheTokenAndItsDeliveryAreQueuedTogether(t *testing.T) {
	store, ctx := newWorkerStore(t)
	gw := newFakeGateway()
	ring := testKeyring(t)
	employee := onboard(t, ctx, ops.New(store), "work1")
	// The export from onboarding has already run and found nothing.
	for {
		c, err := store.Tasks().Claim(ctx, "w", []string{repo.TaskOSSExport}, time.Minute)
		if err != nil {
			break
		}
		store.Tasks().Succeed(ctx, c.ID, "w", "")
	}
	provision, err := store.Tasks().Claim(ctx, "w", []string{repo.TaskGatewayProvision}, time.Minute)
	if err != nil {
		t.Fatalf("claim provision: %v", err)
	}
	h := GatewayProvision{Store: outerTasksFail{store}, Gateway: gw, Keyring: ring}
	if _, err := h.Run(ctx, provision); err != nil {
		t.Fatalf("provision: %v", err)
	}
	open, _ := store.Tasks().ListOpen(ctx, 50)
	for _, task := range open {
		if task.Kind == repo.TaskOSSExport && task.EmployeeID == employee.ID {
			return
		}
	}
	t.Fatal("the token is stored but nothing will deliver it")
}

// staticCatalog is a gateway /model/info with a fixed answer.
type staticCatalog []litellm.Model

func (c staticCatalog) Models(context.Context) ([]litellm.Model, error) { return c, nil }

// An employee allowed every model is held to the unpaused channels while a
// channel is paused, and given every model back -- explicitly, since the
// gateway ignores a missing list -- when it is resumed.
func TestAPausedChannelNarrowsEveryModelAndResumingWidensIt(t *testing.T) {
	store, ctx := newWorkerStore(t)
	service := ops.New(store)
	gateway := newFakeGateway()
	catalog := staticCatalog{
		{Name: "grok-4.6", Info: litellm.ModelInfo{LitellmProvider: "bedrock", CatalogVisible: true}},
		{Name: "qwen3-coder-480b", Info: litellm.ModelInfo{LitellmProvider: "bedrock", CatalogVisible: true}},
		{Name: "gemini-3.1-pro", Info: litellm.ModelInfo{LitellmProvider: "vertex_ai", CatalogVisible: true}},
	}
	w := New(store, Options{Owner: "worker-1", BaseBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	w.Register(repo.TaskGatewayProvision, GatewayProvision{Store: store, Gateway: gateway, Keyring: testKeyring(t), Catalog: catalog})
	w.Register(repo.TaskOSSExport, HandlerFunc(func(context.Context, repo.Task) (Result, error) { return Result{}, nil }))

	e, err := service.Onboard(ctx, ops.OnboardSpec{WindowsUser: "alice", Actor: "zhang",
		Quota: repo.Quota{MonthlyBudget: "20", RPM: 1, TPM: 1, Parallel: 1}}) // no list: every model
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ctx, w)
	user := gateway.users[GatewayUserID("alice")]
	if user.Models == nil || len(user.Models) != 0 {
		t.Fatalf("every model is sent as an explicit empty list, got %#v", user.Models)
	}

	n, err := service.SetChannelPaused(ctx, litellm.ChannelGoogle, true, "403", 0, "zhang", "r1")
	if err != nil || n != 1 {
		t.Fatalf("pause: %d %v", n, err)
	}
	drain(t, ctx, w)
	want := []string{"grok-4.6", "qwen3-coder-480b"}
	if got := gateway.users[GatewayUserID("alice")].Models; !sameSet(got, want) {
		t.Fatalf("paused: user models %v, want %v", got, want)
	}
	for _, k := range gateway.keys {
		if !sameSet(k.Models, want) {
			t.Fatalf("paused: key models %v, want %v", k.Models, want)
		}
	}

	if _, err := service.SetChannelPaused(ctx, litellm.ChannelGoogle, false, "", 1, "zhang", "r2"); err != nil {
		t.Fatal(err)
	}
	drain(t, ctx, w)
	if got := gateway.users[GatewayUserID("alice")].Models; got == nil || len(got) != 0 {
		t.Fatalf("resumed: user models %#v, want an explicit empty list", got)
	}
	for _, k := range gateway.keys {
		if len(k.Models) != 0 {
			t.Fatalf("resumed: key models %v, want every model", k.Models)
		}
	}
	_ = e
}
