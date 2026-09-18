package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// The state nothing else in the system can find: a grant we believe is
// revoked that the gateway is still serving. A working token nobody thinks
// exists.
func TestReconcileFindsATokenWeThoughtWasGone(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)
	alias := KeyAlias("work1", employee.AuthEpoch)

	// Offboard, but the gateway never actually loses the key -- a revoke that
	// reported success and did not take, or somebody putting it back by hand.
	if _, err := service.Offboard(ctx, employee.ID, "zhang", ""); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	drain(t, ctx, w)
	if _, err := gateway.GenerateKey(ctx, alias, GatewayUserID("work1"), nil, nil); err != nil {
		t.Fatalf("put the key back: %v", err)
	}

	// Everything has just been observed, so make that observation stale.
	time.Sleep(5 * time.Millisecond)
	var drifted []string
	handler := Reconcile{
		Store: store, Gateway: gateway, StaleAfter: time.Millisecond,
		OnDrift: func(grant repo.Grant, observed string) {
			drifted = append(drifted, grant.KeyAlias+": "+observed)
		},
	}
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(drifted) != 1 {
		t.Fatalf("drift reported = %v, want the revoked grant the gateway still served", drifted)
	}
	// Found and taken away: this one is repaired, because the intent is
	// unambiguous and leaving it live is the danger.
	if got := gateway.aliases(); len(got) != 0 {
		t.Errorf("the gateway still holds %v", got)
	}
	grant, err := store.Grants().ByKeyAlias(ctx, "", alias)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.Actual != repo.ActualRevoked || grant.ReconciledAt == nil {
		t.Errorf("grant = %+v, want it observed revoked with a time", grant)
	}
}

// The other direction is reported, not repaired: issuing a token is a
// provisioning decision with an epoch attached, and minting one from a
// reconciliation would hand out credentials nobody asked for.
func TestReconcileReportsAMissingTokenWithoutIssuingOne(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)
	alias := KeyAlias("work1", employee.AuthEpoch)

	if err := gateway.DeleteKeyByAlias(ctx, alias); err != nil {
		t.Fatalf("remove the key behind our back: %v", err)
	}
	minted := gateway.minted

	time.Sleep(5 * time.Millisecond)
	var drifted []string
	handler := Reconcile{
		Store: store, Gateway: gateway, StaleAfter: time.Millisecond,
		OnDrift: func(grant repo.Grant, observed string) { drifted = append(drifted, observed) },
	}
	if _, err := handler.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(drifted) != 1 || drifted[0] != "missing from the gateway" {
		t.Fatalf("drift = %v", drifted)
	}
	if gateway.minted != minted {
		t.Error("reconciliation issued a token")
	}
	grant, err := store.Grants().ByKeyAlias(ctx, "", alias)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.Actual != repo.ActualMissing || grant.LastError == "" {
		t.Errorf("grant = %+v, want it recorded missing with a reason", grant)
	}
}

func TestReconcileLeavesAgreementAlone(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	onboard(t, ctx, service, "work1")
	drain(t, ctx, w)

	time.Sleep(5 * time.Millisecond)
	var drifted int
	handler := Reconcile{
		Store: store, Gateway: gateway, StaleAfter: time.Millisecond,
		OnDrift: func(repo.Grant, string) { drifted++ },
	}
	result, err := handler.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// It has to have actually looked: a pass that checked nothing would also
	// report no drift.
	if !strings.Contains(result.Note, "checked 1") {
		t.Errorf("result = %q, want it to say it checked the one grant", result.Note)
	}
	if drifted != 0 {
		t.Errorf("%d grants reported as drifted when everything agreed", drifted)
	}
	if got := gateway.aliases(); len(got) != 1 {
		t.Errorf("reconciliation changed the gateway: %v", got)
	}
}

// A gateway that is answering with errors must not be read as a gateway with
// no keys: recording "missing" for every grant would revoke nothing and alarm
// everybody.
func TestReconcileStopsWhenTheGatewayIsUnwell(t *testing.T) {
	store, service, gateway, _, w, ctx := provisioned(t)
	onboard(t, ctx, service, "work1")
	drain(t, ctx, w)

	time.Sleep(5 * time.Millisecond)
	gateway.failWith["FindKeyByAlias"] = &litellm.APIError{
		Status: 503, Path: "/key/info", Body: "service unavailable"}
	handler := Reconcile{Store: store, Gateway: gateway, StaleAfter: time.Millisecond}
	_, err := handler.Run(ctx, repo.Task{})
	if err == nil {
		t.Fatal("reconciliation reported success against a gateway that was failing")
	}
	if IsPermanent(err) {
		t.Error("a 503 was treated as permanent; it is a fact about this moment")
	}

	grants, err := store.Grants().ByEmployee(ctx, mustEmployee(t, ctx, store, "work1").ID)
	if err != nil {
		t.Fatalf("grants: %v", err)
	}
	for _, grant := range grants {
		if grant.Actual == repo.ActualMissing {
			t.Error("a failing gateway was recorded as not having the key")
		}
	}
}

func TestReconcileIsQueuedOncePerHour(t *testing.T) {
	store, _, _, _, ctx := exporting(t)
	at := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	for range 3 {
		if err := EnqueueReconcile(ctx, store, at); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	tasks, err := store.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	for _, task := range tasks {
		if task.Kind == repo.TaskReconcile {
			count++
		}
	}
	// A scheduler that fires twice, or two consoles that both decide it is
	// time, must produce one task.
	if count != 1 {
		t.Fatalf("%d reconciliation tasks queued for one hour", count)
	}
	if err := EnqueueReconcile(ctx, store, at.Add(time.Hour)); err != nil {
		t.Fatalf("enqueue the next hour: %v", err)
	}
	tasks, err = store.Tasks().ListOpen(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count = 0
	for _, task := range tasks {
		if task.Kind == repo.TaskReconcile {
			count++
		}
	}
	if count != 2 {
		t.Errorf("%d reconciliation tasks after queuing a second hour", count)
	}
}

func mustEmployee(t *testing.T, ctx context.Context, store interface {
	Employees() repo.Employees
}, user string) repo.Employee {
	t.Helper()
	employee, err := store.Employees().ByWindowsUser(ctx, user)
	if err != nil {
		t.Fatalf("employee %s: %v", user, err)
	}
	return employee
}
