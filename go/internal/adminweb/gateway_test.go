package adminweb

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func TestReconcilePairsRosterWithTokens(t *testing.T) {
	users := []model.UserEntry{
		{WindowsUser: "alice", Enabled: true},
		{WindowsUser: "bob", Enabled: true},
	}
	keys := []litellm.Key{
		{KeyAlias: "emp-alice", Models: []string{"grok-4.6"}, Spend: 1.5},
	}

	got := reconcile(users, keys)
	if len(got) != 2 {
		t.Fatalf("expected one row per employee, got %d", len(got))
	}

	alice, bob := got[0], got[1]
	if alice.WindowsUser != "alice" || !alice.HasToken || alice.Spend != 1.5 {
		t.Errorf("alice not paired with her token: %+v", alice)
	}
	if bob.HasToken {
		t.Errorf("bob has no token but was shown as holding one: %+v", bob)
	}
	if alice.Orphaned || bob.Orphaned {
		t.Error("neither row should be flagged; both employees are current")
	}
}

func TestReconcileFlagsTokenHeldByDepartedEmployee(t *testing.T) {
	// This is precisely the state offboarding exists to prevent, so it has
	// to be visible rather than looking like any other provisioned row.
	users := []model.UserEntry{{WindowsUser: "carol", Enabled: false}}
	keys := []litellm.Key{{KeyAlias: "emp-carol", Models: []string{"glm-5"}}}

	got := reconcile(users, keys)
	if len(got) != 1 || !got[0].Orphaned {
		t.Fatalf("a live token for a departed employee must be flagged: %+v", got)
	}
}

func TestReconcileSurfacesTokensWithNoRosterEntry(t *testing.T) {
	// No per-user row would ever show these, so without this they would be
	// invisible and keep working forever.
	users := []model.UserEntry{{WindowsUser: "alice", Enabled: true}}
	keys := []litellm.Key{
		{KeyAlias: "emp-alice"},
		{KeyAlias: "emp-ghost", Models: []string{"grok-4.6"}},
	}

	got := reconcile(users, keys)
	var ghost *gatewayHolder
	for i := range got {
		if got[i].WindowsUser == "ghost" {
			ghost = &got[i]
		}
	}
	if ghost == nil {
		t.Fatalf("token with no roster entry was hidden: %+v", got)
	}
	if !ghost.Orphaned || !ghost.HasToken {
		t.Errorf("orphan row is wrong: %+v", *ghost)
	}
}

func TestReconcileIgnoresNonEmployeeKeys(t *testing.T) {
	// The console's own admin key lives on the gateway too; listing it as a
	// stray employee token would send someone chasing a phantom.
	users := []model.UserEntry{{WindowsUser: "alice", Enabled: true}}
	keys := []litellm.Key{
		{KeyAlias: "emp-alice"},
		{KeyAlias: "windows-control"},
		{KeyAlias: ""},
	}

	got := reconcile(users, keys)
	if len(got) != 1 {
		t.Fatalf("only employee tokens belong in this table: %+v", got)
	}
}

func TestGatewayRefusesWithoutConfiguration(t *testing.T) {
	// Rendering buttons that cannot work is worse than saying why.
	s := &Server{}
	if _, err := s.gateway(); err == nil {
		t.Fatal("expected an error when no gateway URL is set")
	}

	s = &Server{opts: Options{GatewayURL: "https://gw.example"}}
	_, err := s.gateway()
	if err == nil {
		t.Fatal("expected an error when the admin key is missing")
	}

	s = &Server{opts: Options{GatewayURL: "https://gw.example", GatewayAdminKey: "sk-admin"}}
	if _, err := s.gateway(); err != nil {
		t.Fatalf("fully configured gateway should build: %v", err)
	}
}

func TestContextWindowLabel(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{256000, "256K"},
		{128000, "128K"},
		{1048576, "1M"},
		{999, "999"},
		{0, "—"},
	} {
		if got := contextWindowLabel(tc.in); got != tc.want {
			t.Errorf("contextWindowLabel(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
