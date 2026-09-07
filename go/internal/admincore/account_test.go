package admincore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

var testGW = GatewayConfig{BaseURL: "https://gw.example"}

func aliceSpec() AccountSpec {
	return AccountSpec{
		WindowsUser: "Alice", Name: "Alice Wang", Department: "研发",
		Quota:  litellm.Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4},
		Models: []string{"glm-5"},
	}
}

func TestOnboardOpensEverythingInOneStep(t *testing.T) {
	m, store := newManager()
	gw := newFakeGateway()

	if err := m.Onboard(context.Background(), gw, testGW, aliceSpec()); err != nil {
		t.Fatalf("onboard: %v", err)
	}

	// Roster: present, enabled, labelled.
	us, _ := m.LoadUsers()
	e := us.Find("alice")
	if e == nil || !e.Enabled || e.Name != "Alice Wang" || e.Department != "研发" {
		t.Fatalf("roster entry wrong: %+v", e)
	}
	// Gateway user: quota and labels mirrored.
	u, ok := gw.users["emp-alice"]
	if !ok {
		t.Fatal("gateway user not created")
	}
	if u.Quota() != aliceSpec().Quota || u.Alias != "Alice Wang" || u.Department() != "研发" {
		t.Errorf("gateway user not mirrored: %+v", u)
	}
	if len(u.Models) != 1 || u.Models[0] != "glm-5" {
		t.Errorf("user allowlist wrong: %v", u.Models)
	}
	// Token: minted under the user, restricted to the allowlist.
	if len(gw.generated) != 1 || gw.generated[0].UserID != "emp-alice" || len(gw.generated[0].Models) != 1 {
		t.Errorf("token wrong: %+v", gw.generated)
	}
	// Delivery: config and catalog in the credentials archive.
	set := deliveredSet(t, store, "Alice")
	if !strings.Contains(string(set[model.PathCodexConfig]), "sk-emp-alice") {
		t.Errorf("config does not carry the new token")
	}
	if len(set[model.PathCodexModels]) == 0 {
		t.Errorf("catalog not delivered")
	}
	// Audit.
	entries, _ := m.ReadAudit("Alice")
	if len(entries) != 1 || entries[0].Action != AuditOnboard {
		t.Errorf("audit missing: %+v", entries)
	}
}

func TestOnboardIsIdempotent(t *testing.T) {
	m, _ := newManager()
	gw := newFakeGateway()
	if err := m.Onboard(context.Background(), gw, testGW, aliceSpec()); err != nil {
		t.Fatalf("first: %v", err)
	}
	spec := aliceSpec()
	spec.Quota.MonthlyBudgetUSD = 40
	if err := m.Onboard(context.Background(), gw, testGW, spec); err != nil {
		t.Fatalf("second: %v", err)
	}
	us, _ := m.LoadUsers()
	if len(us.Users) != 1 {
		t.Errorf("second onboarding duplicated the roster entry: %+v", us.Users)
	}
	if got := gw.users["emp-alice"].Quota().MonthlyBudgetUSD; got != 40 {
		t.Errorf("quota not updated on re-onboard: %v", got)
	}
	if len(gw.generated) != 2 || len(gw.deleted) != 1 {
		t.Errorf("re-onboard must revoke the old token and mint one new: generated=%d deleted=%v", len(gw.generated), gw.deleted)
	}
}

// CodexAccount/ClaudeAccount are administrator notes, not read by admincore
// itself: Onboard should still write them when given, and a re-onboard that
// omits them (e.g. adminweb's actionAccountReopen, which has no form fields
// for these) must not blank a note nobody re-typed.
func TestOnboardWritesAndPreservesAccountNotes(t *testing.T) {
	m, _ := newManager()
	gw := newFakeGateway()
	spec := aliceSpec()
	spec.CodexAccount, spec.ClaudeAccount = "alice@codex.example", "alice@claude.example"
	if err := m.Onboard(context.Background(), gw, testGW, spec); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	us, _ := m.LoadUsers()
	e := us.Find("alice")
	if e.CodexAccount != "alice@codex.example" || e.ClaudeAccount != "alice@claude.example" {
		t.Fatalf("notes not written: %+v", e)
	}

	// Re-onboard without touching the notes: they must survive.
	again := aliceSpec()
	if err := m.Onboard(context.Background(), gw, testGW, again); err != nil {
		t.Fatalf("re-onboard: %v", err)
	}
	us, _ = m.LoadUsers()
	e = us.Find("alice")
	if e.CodexAccount != "alice@codex.example" || e.ClaudeAccount != "alice@claude.example" {
		t.Fatalf("re-onboard with empty notes blanked them: %+v", e)
	}
}

func TestOnboardReenablesDepartedEmployee(t *testing.T) {
	m, _ := newManager()
	gw := newFakeGateway()
	_ = m.SaveUsers(model.Users{Users: []model.UserEntry{{WindowsUser: "alice", Enabled: false}}})
	if err := m.Onboard(context.Background(), gw, testGW, aliceSpec()); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	us, _ := m.LoadUsers()
	if e := us.Find("alice"); e == nil || !e.Enabled {
		t.Errorf("re-onboarding must re-enable: %+v", e)
	}
}

func TestOnboardRejectsInvalidQuotaBeforeTouchingAnything(t *testing.T) {
	m, store := newManager()
	gw := newFakeGateway()
	spec := aliceSpec()
	spec.Quota.RPM = 0
	if err := m.Onboard(context.Background(), gw, testGW, spec); err == nil {
		t.Fatal("expected an error")
	}
	if len(store.objects) != 0 || len(gw.upserts) != 0 || len(gw.generated) != 0 {
		t.Error("an invalid spec must not write anywhere")
	}
}

func TestOnboardStopsVisiblyWhenUserWriteFails(t *testing.T) {
	// Roster written, no user, no token: the list page shows "在职无令牌"
	// and re-running onboard is the fix.
	m, _ := newManager()
	gw := newFakeGateway()
	gw.upsertErr = errors.New("gateway down")
	err := m.Onboard(context.Background(), gw, testGW, aliceSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	us, _ := m.LoadUsers()
	if e := us.Find("alice"); e == nil || !e.Enabled {
		t.Errorf("roster should already be written: %+v", e)
	}
	if len(gw.generated) != 0 {
		t.Error("no token must be minted without a user")
	}
	if entries, _ := m.ReadAudit("alice"); len(entries) != 0 {
		t.Error("a failed onboarding is not audited as done")
	}
}

func TestOnboardWithdrawsTokenWhenDeliveryFails(t *testing.T) {
	m, store := newManager()
	gw := newFakeGateway()
	store.putErr = nil
	// Fail only the credentials.zip write: let roster and audit through.
	// credsKey preserves the caller's casing (it does not lower-case, unlike
	// AuditKey/KeyAlias), and aliceSpec's WindowsUser is "Alice", so the key
	// under test must match that casing or the write never actually fails.
	store.putErrFor = credsKey("Alice")
	err := m.Onboard(context.Background(), gw, testGW, aliceSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(gw.generated) != 1 || len(gw.deleted) != 1 {
		t.Errorf("token minted then not withdrawn: generated=%d deleted=%v", len(gw.generated), gw.deleted)
	}
}

func onboarded(t *testing.T) (*Manager, *fakeStore, *fakeGateway) {
	t.Helper()
	m, store := newManager()
	gw := newFakeGateway()
	if err := m.Onboard(context.Background(), gw, testGW, aliceSpec()); err != nil {
		t.Fatalf("seed onboard: %v", err)
	}
	return m, store, gw
}

func TestOffboardRevokesEverythingAndKeepsHistory(t *testing.T) {
	m, store, gw := onboarded(t)
	if err := m.BindMachine("PC-1", "alice", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := m.BindMachine("PC-2", "Alice", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}

	if err := m.Offboard(context.Background(), gw, "alice"); err != nil {
		t.Fatalf("offboard: %v", err)
	}

	us, _ := m.LoadUsers()
	if e := us.Find("alice"); e == nil || e.Enabled {
		t.Errorf("roster must keep the entry, disabled: %+v", e)
	}
	if _, live := gw.existing["emp-alice"]; live {
		t.Error("token still live")
	}
	if _, ok := store.objects[credsKey("Alice")]; ok {
		t.Error("credentials.zip still in the store; the agent would never clear the machine")
	}
	bindings, _ := m.ListBindings()
	if len(bindings) != 0 {
		t.Errorf("machines still bound: %v", bindings)
	}
	if _, ok := gw.users["emp-alice"]; !ok {
		t.Error("gateway user must be kept for its spend history")
	}
	entries, _ := m.ReadAudit("alice")
	if len(entries) != 2 || entries[0].Action != AuditOffboard {
		t.Errorf("audit: %+v", entries)
	}
}

func TestOffboardContinuesPastGatewayFailure(t *testing.T) {
	// The gateway being down must not leave the files on the machine.
	m, store, gw := onboarded(t)
	_ = m.BindMachine("PC-1", "alice", "")
	gw.deleteAliasErr = errors.New("gateway down")

	err := m.Offboard(context.Background(), gw, "alice")
	if err == nil {
		t.Fatal("a failed revocation must be reported")
	}
	us, _ := m.LoadUsers()
	if e := us.Find("alice"); e.Enabled {
		t.Error("roster must be disabled even when the gateway fails")
	}
	if _, ok := store.objects[credsKey("Alice")]; ok {
		t.Error("credentials must be deleted even when the gateway fails")
	}
	if b, _ := m.ListBindings(); len(b) != 0 {
		t.Error("machines must be unbound even when the gateway fails")
	}
	if _, live := gw.existing["emp-alice"]; !live {
		t.Error("test setup: token should still be live so the list flags it")
	}
	if entries, _ := m.ReadAudit("alice"); len(entries) != 1 || entries[0].Action != AuditOnboard {
		t.Errorf("a partial offboard must not be audited as done: %+v", entries)
	}
}

func TestOffboardUnknownUserIsAnError(t *testing.T) {
	m, _ := newManager()
	if err := m.Offboard(context.Background(), newFakeGateway(), "ghost"); err == nil {
		t.Fatal("offboarding someone not on the roster must be refused")
	}
}

func TestOffboardWithoutTokenSucceeds(t *testing.T) {
	m, _ := newManager()
	_ = m.SaveUsers(model.Users{Users: []model.UserEntry{{WindowsUser: "alice", Enabled: true}}})
	if err := m.Offboard(context.Background(), newFakeGateway(), "alice"); err != nil {
		t.Fatalf("no token is not a failure: %v", err)
	}
}

func TestSetQuotaUpdatesUserOnly(t *testing.T) {
	m, _, gw := onboarded(t)
	q := litellm.Quota{MonthlyBudgetUSD: 50, RPM: 120, TPM: 400000, Parallel: 8}
	if err := m.SetQuota(context.Background(), gw, "alice", q); err != nil {
		t.Fatalf("quota: %v", err)
	}
	if gw.users["emp-alice"].Quota() != q {
		t.Errorf("quota not applied: %+v", gw.users["emp-alice"].Quota())
	}
	if len(gw.generated) != 1 {
		t.Error("changing a quota must not re-issue the token")
	}
	if len(gw.users["emp-alice"].Models) != 1 {
		t.Errorf("quota change must keep the allowlist: %v", gw.users["emp-alice"].Models)
	}
	if entries, _ := m.ReadAudit("alice"); entries[0].Action != AuditQuota || entries[0].Detail["budget"] != float64(50) {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestSetQuotaRequiresAnAccount(t *testing.T) {
	m, _ := newManager()
	_ = m.SaveUsers(model.Users{Users: []model.UserEntry{{WindowsUser: "alice", Enabled: true}}})
	err := m.SetQuota(context.Background(), newFakeGateway(), "alice", DefaultQuota)
	if err == nil {
		t.Fatal("no gateway user yet: the fix is onboarding, not a quota change")
	}
}

func TestSetModelsUpdatesUserTokenAndCatalog(t *testing.T) {
	m, store, gw := onboarded(t)
	if err := m.SetModels(context.Background(), gw, testGW, "alice", []string{"grok-4.6"}); err != nil {
		t.Fatalf("models: %v", err)
	}
	if got := gw.users["emp-alice"].Models; len(got) != 1 || got[0] != "grok-4.6" {
		t.Errorf("user allowlist: %v", got)
	}
	if got := gw.updated["hash-of-emp-alice"]; len(got) != 1 || got[0] != "grok-4.6" {
		t.Errorf("token allowlist: %v", gw.updated)
	}
	set := deliveredSet(t, store, "alice")
	if !strings.Contains(string(set[model.PathCodexModels]), `"slug": "grok-4.6"`) || strings.Contains(string(set[model.PathCodexModels]), `"slug": "glm-5"`) {
		t.Errorf("catalog not refreshed")
	}
	if entries, _ := m.ReadAudit("alice"); entries[0].Action != AuditModels {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestReissueMintsNewTokenAndAudits(t *testing.T) {
	m, _, gw := onboarded(t)
	if err := m.Reissue(context.Background(), gw, testGW, "alice"); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if len(gw.generated) != 2 || len(gw.deleted) != 1 {
		t.Errorf("reissue must revoke then mint: generated=%d deleted=%v", len(gw.generated), gw.deleted)
	}
	if entries, _ := m.ReadAudit("alice"); entries[0].Action != AuditReissue {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestUnbindUserContinuesPastOneFailure(t *testing.T) {
	m, store := newManager()
	_ = m.SaveUsers(model.Users{Users: []model.UserEntry{{WindowsUser: "alice", Enabled: true}}})
	if err := m.BindMachine("PC-1", "alice", ""); err != nil {
		t.Fatalf("bind PC-1: %v", err)
	}
	if err := m.BindMachine("PC-2", "alice", ""); err != nil {
		t.Fatalf("bind PC-2: %v", err)
	}
	store.deleteErrFor = ossclient.BindingKey("PC-1")

	n, err := m.unbindUser("alice")
	if err == nil {
		t.Fatal("the failed deletion must be reported")
	}
	if n != 1 {
		t.Errorf("expected 1 successful unbind, got %d", n)
	}
	if _, ok := store.objects[ossclient.BindingKey("PC-2")]; ok {
		t.Error("PC-2 should have been unbound despite PC-1 failing")
	}
	if _, ok := store.objects[ossclient.BindingKey("PC-1")]; !ok {
		t.Error("PC-1's binding should remain since its delete failed")
	}
}
