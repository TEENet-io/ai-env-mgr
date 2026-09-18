package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"

	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// fakeObjects is an in-memory OSS.
type fakeObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
}

func newFakeObjects() *fakeObjects { return &fakeObjects{objects: map[string][]byte{}} }

func (f *fakeObjects) Get(key string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, "", ossclient.ErrNotFound
	}
	return data, "etag", nil
}

func (f *fakeObjects) Put(key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[key] = data
	return nil
}

func (f *fakeObjects) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeObjects) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	return data, ok
}

// fakeCatalog is the gateway's model list.
type fakeCatalog struct{ names []string }

func (c fakeCatalog) Models(context.Context) ([]litellm.Model, error) {
	out := make([]litellm.Model, 0, len(c.names))
	for _, name := range c.names {
		out = append(out, litellm.Model{Name: name, Info: litellm.ModelInfo{
			DisplayName: name, ContextWindow: 200000, CatalogVisible: true,
		}})
	}
	return out, nil
}

// exporting wires a worker with all three handlers over a fake gateway and a
// fake object store, which between them are the whole delivery path.
func exporting(t *testing.T) (*dbstore.Store, *ops.Service, *fakeObjects, *Worker, context.Context) {
	t.Helper()
	store, service, gateway, ring, w, ctx := provisioned(t)
	objects := newFakeObjects()
	w.Register(repo.TaskOSSExport, OSSExport{
		Store: store, Objects: objects, Keyring: ring,
		Catalog:        fakeCatalog{names: []string{"claude-4.5-sonnet", "gemini-2.5-pro"}},
		GatewayBaseURL: "https://litellm.teenet.app",
	})
	_ = gateway
	return store, service, objects, w, ctx
}

func TestTheExportPutsAWorkingConfigWhereTheAgentLooks(t *testing.T) {
	_, service, objects, w, ctx := exporting(t)
	onboard(t, ctx, service, "work1")
	drain(t, ctx, w)

	blob, ok := objects.get(ossclient.UserKey("work1", "credentials.zip"))
	if !ok {
		t.Fatal("nothing was published for work1")
	}
	set, err := creds.Unpack(blob)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	config := string(set[model.PathCodexConfig])
	if config == "" {
		t.Fatal("the archive has no config.toml")
	}
	// The base URL in this file is what Codex on a desktop connects to. A
	// loopback address here silently breaks every machine -- which is exactly
	// what happened once.
	if !strings.Contains(config, `base_url = "https://litellm.teenet.app/v1"`) {
		t.Errorf("config.toml points at the wrong gateway:\n%s", config)
	}
	if !strings.Contains(config, "experimental_bearer_token") || strings.Contains(config, `experimental_bearer_token = ""`) {
		t.Errorf("config.toml carries no token:\n%s", config)
	}
	if len(set[model.PathCodexModels]) == 0 {
		t.Error("the archive has no models.json, so the picker would be empty")
	}
}

func TestTheExportKeepsWhatItDidNotPublishAndDropsWhatNobodyPublishesAnyMore(t *testing.T) {
	_, service, objects, w, ctx := exporting(t)

	// An archive from before: an entry this export does not produce, which
	// must survive, and the Claude login an earlier console published, which
	// must not -- Claude Code is no longer managed.
	existing, err := creds.Pack(model.CredentialSet{
		model.PathCodexAuth:        []byte(`{"auth_mode":"chatgpt"}`),
		"claude/.credentials.json": []byte(`{"claude":"token"}`),
		"claude.json":              []byte(`{"hasCompletedOnboarding":true}`),
	})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if err := objects.Put(ossclient.UserKey("work1", "credentials.zip"), existing); err != nil {
		t.Fatalf("seed: %v", err)
	}

	onboard(t, ctx, service, "work1")
	drain(t, ctx, w)

	blob, _ := objects.get(ossclient.UserKey("work1", "credentials.zip"))
	set, err := creds.Unpack(blob)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if string(set[model.PathCodexAuth]) != `{"auth_mode":"chatgpt"}` {
		t.Error("publishing the Codex config dropped an entry it did not produce")
	}
	if len(set[model.PathCodexConfig]) == 0 {
		t.Error("the Codex config was not published")
	}
	for _, legacy := range model.LegacyClaudeEntries {
		if _, ok := set[legacy]; ok {
			t.Errorf("%s is still in the archive; the machine would keep a login for a tool nobody manages", legacy)
		}
	}
}

func TestOffboardingTakesTheCredentialsOffTheMachine(t *testing.T) {
	_, service, objects, w, ctx := exporting(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)
	if _, ok := objects.get(ossclient.UserKey("work1", "credentials.zip")); !ok {
		t.Fatal("nothing was published to begin with")
	}

	if _, err := service.Offboard(ctx, employee.ID, "zhang", ""); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	drain(t, ctx, w)

	// The object going away is how the revocation reaches the desktop: the
	// agent deletes the local credentials and ends the Codex session that
	// still holds the token in memory.
	if _, ok := objects.get(ossclient.UserKey("work1", "credentials.zip")); ok {
		t.Error("the credentials are still published after offboarding")
	}
}

func TestBindingAndRestartReachTheMachine(t *testing.T) {
	_, service, objects, w, ctx := exporting(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)

	if _, err := service.BindMachine(ctx, "DESKTOP-01", employee.ID, "第一台", "zhang", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	drain(t, ctx, w)

	raw, ok := objects.get(ossclient.BindingKey("DESKTOP-01"))
	if !ok {
		t.Fatal("no binding object was written")
	}
	var binding model.Binding
	if err := json.Unmarshal(raw, &binding); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	if binding.User != "work1" || binding.Note != "第一台" {
		t.Errorf("binding = %+v", binding)
	}
	if binding.RestartCodex != "" {
		t.Error("a fresh binding carries a restart request")
	}

	nonce, err := service.RequestCodexRestart(ctx, "desktop-01", "zhang", "")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	drain(t, ctx, w)
	raw, _ = objects.get(ossclient.BindingKey("DESKTOP-01"))
	if err := json.Unmarshal(raw, &binding); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	if binding.RestartCodex != nonce {
		t.Errorf("the restart nonce did not reach the machine: %q, want %q", binding.RestartCodex, nonce)
	}

	if err := service.UnbindMachine(ctx, "desktop-01", "zhang", ""); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	drain(t, ctx, w)
	// The agent reads the absence as "not assigned yet" and keeps applying the
	// machine-wide policy, which is right for a machine that has been taken
	// back.
	if _, ok := objects.get(ossclient.BindingKey("DESKTOP-01")); ok {
		t.Error("the binding object survived unbinding")
	}
}

func TestThePolicyExportAlwaysWritesTheCurrentOne(t *testing.T) {
	store, service, objects, w, ctx := exporting(t)

	if _, err := service.PublishPolicy(ctx,
		[]byte(`{"blockEnabled":true,"syncIntervalMinutes":30}`), "first", "zhang", ""); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Two publishes before either export runs. The older task must not be able
	// to put the earlier policy back after the newer one landed.
	if _, err := service.PublishPolicy(ctx,
		[]byte(`{"blockEnabled":false,"syncIntervalMinutes":5}`), "second", "zhang", ""); err != nil {
		t.Fatalf("publish again: %v", err)
	}
	drain(t, ctx, w)

	raw, ok := objects.get(ossclient.PolicyKey())
	if !ok {
		t.Fatal("no policy object was written")
	}
	var published model.Policy
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if published.SyncIntervalMinutes != 5 || published.BlockEnabled {
		t.Errorf("published policy = %+v, want the second one", published)
	}
	current, err := store.Policies().Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current.Version == 0 {
		t.Error("no policy is current")
	}
}

func TestAnOSSFailureDelaysTheDeliveryRatherThanLosingIt(t *testing.T) {
	store, service, objects, w, ctx := exporting(t)
	objects.putErr = errors.New("connection reset by peer")
	employee := onboard(t, ctx, service, "work1")

	for range 4 {
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	var export repo.Task
	tasks, err := store.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, task := range tasks {
		if task.Kind == repo.TaskOSSExport {
			export = task
		}
	}
	if export.Status != repo.TaskRetryWait {
		t.Fatalf("the export is %s, want it waiting to be retried", export.Status)
	}

	// The credentials exist in the database the whole time; only the delivery
	// is late.
	if _, err := store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway); err != nil {
		t.Fatalf("the credential was lost while the delivery failed: %v", err)
	}

	objects.putErr = nil
	time.Sleep(30 * time.Millisecond)
	drain(t, ctx, w)
	if _, ok := objects.get(ossclient.UserKey("work1", "credentials.zip")); !ok {
		t.Error("the delivery never happened after OSS recovered")
	}
}

func TestForgettingAMachineRemovesItsObjects(t *testing.T) {
	_, service, objects, w, ctx := exporting(t)
	employee := onboard(t, ctx, service, "work1")
	if _, err := service.BindMachine(ctx, "DESKTOP-01", employee.ID, "", "zhang", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	objects.mu.Lock()
	objects.objects[ossclient.StatusKey("DESKTOP-01")] = []byte(`{"machine":"DESKTOP-01"}`)
	objects.mu.Unlock()
	drain(t, ctx, w)

	if err := service.ForgetMachine(ctx, "desktop-01", "zhang", ""); err != nil {
		t.Fatalf("forget: %v", err)
	}
	drain(t, ctx, w)
	if _, ok := objects.get(ossclient.BindingKey("DESKTOP-01")); ok {
		t.Error("the binding object survived forgetting")
	}
	if _, ok := objects.get(ossclient.StatusKey("DESKTOP-01")); ok {
		t.Error("the status object survived forgetting")
	}
}
