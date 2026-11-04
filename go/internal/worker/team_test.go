package worker

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// A console that starts sharing its gateway: employees provisioned before it
// had a team keep working on their pre-team user until they are moved, and
// moving issues the next token under the team's user.
func TestMovingToATeamReissuesUnderTheTeamUser(t *testing.T) {
	store, service, gateway, ring, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)
	if _, ok := gateway.users["emp-work1"]; !ok {
		t.Fatalf("pre-team user missing: %v", gateway.users)
	}

	// The console restarts with a team.
	w.Register(repo.TaskGatewayProvision, GatewayProvision{Store: store, Gateway: gateway, Keyring: ring, TeamID: "acme"})

	// A quota change before the move changes the user the live token draws
	// on, not a new one beside it.
	q, err := store.Quotas().Get(ctx, employee.ID)
	if err != nil {
		t.Fatal(err)
	}
	q.MonthlyBudget = "80"
	if err := service.SetQuota(ctx, employee.ID, q, q.Version, "zhang", ""); err != nil {
		t.Fatal(err)
	}
	drain(t, ctx, w)
	if got := gateway.users["emp-work1"].Quota.MonthlyBudgetUSD; got != 80 {
		t.Errorf("pre-team user budget = %v, want 80", got)
	}
	if _, made := gateway.users["acme-emp-work1"]; made {
		t.Fatal("a quota change created the team user before the move")
	}

	dry, err := service.MoveToTeam(ctx, "acme", true, "migrate", "")
	if err != nil || len(dry) != 1 || dry[0] != "work1" {
		t.Fatalf("dry run = %v, %v", dry, err)
	}
	if again, _ := store.Employees().ByID(ctx, employee.ID); again.AuthEpoch != employee.AuthEpoch {
		t.Fatal("the dry run re-issued")
	}

	moved, err := service.MoveToTeam(ctx, "acme", false, "migrate", "")
	if err != nil || len(moved) != 1 {
		t.Fatalf("move = %v, %v", moved, err)
	}
	drain(t, ctx, w)

	current, err := store.Employees().ByID(ctx, employee.ID)
	if err != nil {
		t.Fatal(err)
	}
	alias := KeyAlias("acme", "work1", current.ID, current.AuthEpoch)
	if got := gateway.aliases(); len(got) != 1 || got[0] != alias {
		t.Fatalf("gateway holds %v, want only %s", got, alias)
	}
	grant, err := store.Grants().Active(ctx, "", employee.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grant.ExternalUser != "acme-emp-work1" {
		t.Errorf("grant user = %q", grant.ExternalUser)
	}
	if got := gateway.users["acme-emp-work1"].Quota.MonthlyBudgetUSD; got != 80 {
		t.Errorf("team user budget = %v, want the stored 80", got)
	}
	// The pre-team user stays: it holds the spend history, and deleting a
	// user deletes its keys.
	if _, ok := gateway.users["emp-work1"]; !ok {
		t.Error("the move deleted the pre-team user")
	}

	if again, err := service.MoveToTeam(ctx, "acme", false, "migrate", ""); err != nil || len(again) != 0 {
		t.Fatalf("second move = %v, %v", again, err)
	}
}

func TestDeletingAMovedAccountRemovesBothUsers(t *testing.T) {
	store, ctx := newWorkerStore(t)
	gw := newFakeGateway()
	e, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	if err != nil {
		t.Fatal(err)
	}
	gw.users["emp-work1"] = litellm.UserSpec{UserID: "emp-work1"}
	gw.users["acme-emp-work1"] = litellm.UserSpec{UserID: "acme-emp-work1"}
	cred, _ := store.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: e.ID, Epoch: 1, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	if _, err := store.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: e.ID, Epoch: 1, ExternalUser: "emp-work1", KeyAlias: "emp-work1-e1", CredentialID: cred.ID}); err != nil {
		t.Fatal(err)
	}
	// Somebody else's user on the shared gateway, with the same Windows name.
	gw.users["other-emp-work1"] = litellm.UserSpec{UserID: "other-emp-work1"}

	h := GatewayDelete{Store: store, Gateway: gw, TeamID: "acme"}
	task := repo.Task{Payload: mustJSON(t, map[string]any{"employee_id": e.ID, "windows_user": "work1"})}
	if _, err := h.Run(ctx, task); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, id := range []string{"emp-work1", "acme-emp-work1"} {
		if _, still := gw.users[id]; still {
			t.Errorf("%s is still there", id)
		}
	}
	if _, ok := gw.users["other-emp-work1"]; !ok {
		t.Fatal("deleted another customer's employee")
	}
}
