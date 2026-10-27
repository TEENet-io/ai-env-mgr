package ops

import (
	"errors"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestDeliverCatalogTestFirstThenEverybody(t *testing.T) {
	svc, store, ctx := newService(t)
	withToken := func(user string) repo.Employee {
		e, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: user})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: e.AuthEpoch,
			Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"}); err != nil {
			t.Fatal(err)
		}
		return e
	}
	alice, bob := withToken("alice"), withToken("bob")
	fresh, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "fresh"}) // no token yet
	gone := withToken("gone")
	if _, err := store.Employees().Offboard(ctx, gone.ID, gone.Version); err != nil {
		t.Fatal(err)
	}
	snap := SnapshotOf([]litellm.Model{{Name: "gpt-5"}, {Name: "claude-5"}})
	if snap.Models[0] != "claude-5" || snap.Digest == "" {
		t.Fatalf("snapshot = %+v", snap)
	}

	// A test delivery goes to one person and does not move the baseline.
	if n, err := svc.DeliverCatalog(ctx, snap, alice.ID, 0, "admin", "r1"); err != nil || n != 1 {
		t.Fatalf("test delivery: %d %v", n, err)
	}
	state, version, _ := svc.CatalogDeliveryState(ctx)
	if state.Last.Scope != "test" || len(state.Last.Tasks) != 1 || state.FleetAt != "" || version != 1 {
		t.Fatalf("after test: %+v v%d", state, version)
	}
	if _, err := svc.DeliverCatalog(ctx, snap, fresh.ID, version, "admin", "r2"); err == nil {
		t.Fatal("somebody without a token has no configuration to deliver")
	}

	// Everybody: the two with tokens, not the one without, not the leaver.
	if n, err := svc.DeliverCatalog(ctx, snap, "", version, "admin", "r3"); err != nil || n != 2 {
		t.Fatalf("fleet delivery: %d %v", n, err)
	}
	state, version, _ = svc.CatalogDeliveryState(ctx)
	if state.Last.Scope != "all" || state.FleetAt == "" || len(state.Fleet.Models) != 2 || state.Last.Tasks[bob.ID] == "" {
		t.Fatalf("after fleet: %+v", state)
	}
	// A page rendered before somebody else delivered is refused.
	if _, err := svc.DeliverCatalog(ctx, snap, "", version-1, "admin", "r4"); !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("stale delivery: %v", err)
	}
}

func TestCatalogSnapshotDetectsAddMetadataUpdateAndRemoval(t *testing.T) {
	before := SnapshotOf([]litellm.Model{{Name: "model-a", Info: litellm.ModelInfo{DisplayName: "A"}}})
	added := SnapshotOf([]litellm.Model{{Name: "model-a", Info: litellm.ModelInfo{DisplayName: "A"}}, {Name: "model-b", Info: litellm.ModelInfo{DisplayName: "B"}}})
	updated := SnapshotOf([]litellm.Model{{Name: "model-a", Info: litellm.ModelInfo{DisplayName: "A updated"}}, {Name: "model-b", Info: litellm.ModelInfo{DisplayName: "B"}}})
	removed := SnapshotOf([]litellm.Model{{Name: "model-b", Info: litellm.ModelInfo{DisplayName: "B"}}})
	if before.Digest == added.Digest || added.Digest == updated.Digest || updated.Digest == removed.Digest {
		t.Fatalf("catalog changes must produce distinct snapshots: %v %v %v %v", before, added, updated, removed)
	}
}

func TestDeliverCatalogBatchSelectsEmployeesAndDevicesAndRetriesOneEmployee(t *testing.T) {
	svc, store, ctx := newService(t)
	withToken := func(user string) repo.Employee {
		e, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: user})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: e.AuthEpoch,
			Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"}); err != nil {
			t.Fatal(err)
		}
		return e
	}
	alice, bob := withToken("alice"), withToken("bob")
	devices := make([]repo.Device, 3)
	for i, name := range []string{"PC-A1", "PC-A2", "PC-B"} {
		var err error
		devices[i], err = store.Devices().EnsureByHostname(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		owner := alice.ID
		if i == 2 {
			owner = bob.ID
		}
		if _, err := store.Bindings().Bind(ctx, devices[i].ID, owner, "", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := SnapshotOf([]litellm.Model{{Name: "model-a"}, {Name: "model-b"}})
	if n, err := svc.DeliverCatalogBatch(ctx, snapshot, []string{alice.ID}, []string{devices[2].ID, devices[2].ID}, 0, "admin", "batch"); err != nil || n != 2 {
		t.Fatalf("batch: n=%d err=%v", n, err)
	}
	state, version, err := svc.CatalogDeliveryState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Last.Scope != "batch" || len(state.Last.Tasks) != 2 || len(state.Last.Targets) != 3 || state.FleetAt != "" {
		t.Fatalf("wrong batch targets: %+v", state)
	}
	previous := state.Last.Tasks[bob.ID]
	for range 2 {
		task, err := store.Tasks().Claim(ctx, "test-worker", []string{repo.TaskOSSExport}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if task.ID == previous {
			if _, err := store.Tasks().FailPermanently(ctx, task.ID, "test-worker", "test", "export failed", ""); err != nil {
				t.Fatal(err)
			}
		} else if err := store.Tasks().Succeed(ctx, task.ID, "test-worker", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RetryCatalogDelivery(ctx, snapshot, bob.ID, version, "admin", "retry"); err != nil {
		t.Fatal(err)
	}
	state, version, err = svc.CatalogDeliveryState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Last.Tasks[bob.ID] == previous || state.Last.Tasks[alice.ID] == "" || state.Last.Retries[bob.ID] != 1 || len(state.Last.Targets) != 3 {
		t.Fatalf("retry changed wrong targets: %+v", state.Last)
	}
	if err := svc.RetryCatalogDelivery(ctx, SnapshotOf([]litellm.Model{{Name: "model-c"}}), bob.ID, version, "admin", "stale-catalog"); err == nil {
		t.Fatal("a changed gateway must not be retried under the old snapshot")
	}
	if _, err := svc.DeliverCatalogBatch(ctx, snapshot, nil, []string{devices[0].ID}, version, "admin", "device-only"); err != nil {
		t.Fatal(err)
	}
	state, _, err = svc.CatalogDeliveryState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Last.Targets) != 1 || state.Last.Targets[0].DeviceID != devices[0].ID || len(state.History) != 1 {
		t.Fatalf("device-only selection or history lost: %+v", state)
	}
}
