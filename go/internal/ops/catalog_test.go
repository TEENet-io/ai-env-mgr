package ops

import (
	"errors"
	"testing"

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
