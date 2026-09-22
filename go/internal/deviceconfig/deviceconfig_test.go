package deviceconfig

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func newStore(t *testing.T) (*dbstore.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN is not set; skipping the PostgreSQL integration tests")
	}
	ctx := context.Background()
	dsn, err := dbstore.TestDatabaseDSN(ctx, dsn, "aienv_test_deviceconfig")
	if err != nil {
		t.Fatalf("test database: %v", err)
	}
	database, err := dbstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(database.Close)
	if _, err := database.Pool().Exec(ctx, `drop schema public cascade; create schema public; grant all on schema public to public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return dbstore.NewStore(database), ctx
}

func TestBuildDescribesAMachineAndChangesWhenItDoes(t *testing.T) {
	store, ctx := newStore(t)
	device, _ := store.Devices().EnsureByHostname(ctx, "PC-7")
	if cfg, err := Build(ctx, store, device.ID); err != nil || cfg.HasPolicy {
		t.Fatalf("without a policy: %+v %v", cfg, err)
	}
	policy, _ := json.Marshal(model.Policy{BlockEnabled: true, BlockedDomains: []string{"openai.com"}, SyncIntervalMinutes: 5})
	if _, err := store.Policies().Publish(ctx, policy, "first", "admin"); err != nil {
		t.Fatal(err)
	}

	bare, err := Build(ctx, store, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bare.HasBinding || bare.Bound || bare.PolicyVersion != 1 || !bare.HasPolicy || !bare.Policy.BlockEnabled || bare.ETag == "" || bare.Hostname != "PC-7" {
		t.Fatalf("bare machine = %+v", bare)
	}

	// Binding an employee changes the configuration; the assignee's live
	// credential is what the credentials etag names.
	e, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	store.Bindings().Bind(ctx, device.ID, e.ID, "desk 4", "admin")
	bound, err := Build(ctx, store, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bound.HasBinding || !bound.Bound || bound.Binding.User != "work1" || bound.Binding.Note != "desk 4" || bound.CredentialsETag != "" || bound.AssigneeEmployeeID != e.ID {
		t.Fatalf("bound machine = %+v", bound)
	}
	if bound.ETag == bare.ETag {
		t.Fatal("binding must change the etag")
	}
	cred, _ := store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: e.AuthEpoch, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	withCreds, _ := Build(ctx, store, device.ID)
	if withCreds.CredentialsETag != cred.ID || withCreds.ETag == bound.ETag {
		t.Fatalf("a new credential must show in the etag: %+v", withCreds)
	}

	// The rendered binding object is what the OSS exporter has always
	// written: the same fields an old agent reads.
	obj, _ := withCreds.BindingObject()
	var b model.Binding
	if err := json.Unmarshal(obj, &b); err != nil || b.User != "work1" || b.BoundAt == "" {
		t.Fatalf("binding object = %s (%v)", obj, err)
	}
	if strings.Contains(string(obj), "etag") || strings.Contains(string(obj), "policy") {
		t.Fatal("the binding object carries only the binding")
	}

	// "Sync now" alone makes an unbound machine have a binding object.
	store.Bindings().Unbind(ctx, device.ID, "admin")
	if again, _ := Build(ctx, store, device.ID); again.HasBinding {
		t.Fatal("unbound and untargeted: no binding")
	}
	store.Devices().RequestSync(ctx, device.ID, "n1")
	if again, _ := Build(ctx, store, device.ID); !again.HasBinding || again.Binding.SyncRequested != "n1" || again.Bound {
		t.Fatalf("sync now alone: %+v", again)
	}

	// Forgotten machines say so and nothing else.
	store.Devices().Revoke(ctx, device.ID)
	gone, err := Build(ctx, store, device.ID)
	if err != nil || !gone.Forgotten || gone.HasBinding {
		t.Fatalf("forgotten = %+v %v", gone, err)
	}
}
