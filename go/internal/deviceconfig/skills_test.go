package deviceconfig

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/companyskills"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestSkillsAreTargetedAndRevokeOnUnbindAndOffboard(t *testing.T) {
	store, ctx := newStore(t)
	a, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	b, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work2"})
	d1, _ := store.Devices().EnsureByHostname(ctx, "PC-1")
	d2, _ := store.Devices().EnsureByHostname(ctx, "PC-2")
	store.Bindings().Bind(ctx, d1.ID, a.ID, "", "admin")
	store.Bindings().Bind(ctx, d2.ID, b.ID, "", "admin")
	before, _ := Build(ctx, store, d1.ID)
	unrelated, _ := Build(ctx, store, d2.ID)
	bundle, err := companyskills.FromForm("bippo-test", "测试流程", "做清单时", "先读取模板，再生成结果", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := companyskills.Apply(ctx, store, companyskills.Change{Action: "save", Name: bundle.Name, Title: "测试", Bundle: bundle, TrialEmployee: a.ID, ActorType: "admin", ActorID: "ops"}); err != nil {
		t.Fatal(err)
	}
	after, err := Build(ctx, store, d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Build(ctx, store, d2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ETag == before.ETag || len(after.Skills.Skills) != 1 || after.Skills.User != "work1" {
		t.Fatal("target configuration did not change")
	}
	if other.ETag != unrelated.ETag || len(other.Skills.Skills) != 0 {
		t.Fatal("unrelated employee received skill data or changed ETag")
	}
	if _, err := store.Employees().Offboard(ctx, a.ID, a.Version); err != nil {
		t.Fatal(err)
	}
	closed, err := Build(ctx, store, d1.ID)
	if err != nil || closed.Skills == nil || len(closed.Skills.Skills) != 0 {
		t.Fatalf("offboard: %+v %v", closed, err)
	}
	store.Bindings().Unbind(ctx, d1.ID, "ops")
	unbound, err := Build(ctx, store, d1.ID)
	if err != nil || unbound.Skills == nil || len(unbound.Skills.Skills) != 0 {
		t.Fatalf("unbind: %+v %v", unbound, err)
	}
}
