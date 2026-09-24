package adminweb

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestMachinePageTellsTheMachinesStory(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	store := s.dbm.store
	csrf := csrfFrom(t, s, cookie, "/users")
	for _, u := range []string{"alice", "bob"} {
		dbPost(t, h, "/users/onboard", url.Values{"csrf": {csrf}, "windowsUser": {u}, "email": {"t@example.com"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}, cookie)
	}
	device, _ := store.Devices().EnsureByHostname(ctx, "PC-STORY")
	store.Devices().MarkSeen(ctx, device.ID, "1.2.16", time.Now())
	alice, _ := store.Employees().ByWindowsUser(ctx, "alice")
	bob, _ := store.Employees().ByWindowsUser(ctx, "bob")
	if _, err := s.dbm.ops.BindMachine(ctx, "PC-STORY", alice.ID, "first", "admin", "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.dbm.ops.BindMachine(ctx, "PC-STORY", bob.ID, "second", "admin", "r2"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.dbm.ops.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.42.0",
		SHA256: strings.Repeat("c", 64), SizeBytes: 1, ObjectKey: "k", CreatedBy: "t"}, "t", "r")
	rollout, _ := s.dbm.ops.CreateRollout(ctx, ops.RolloutSpec{Product: repo.ProductCodex, ArtifactID: a.ID, DeviceIDs: []string{device.ID}, Kind: repo.RolloutRelease, Actor: "admin", RequestID: "r"})
	target, _ := store.Releases().OpenTarget(ctx, device.ID, repo.ProductCodex)
	store.Releases().FinishTarget(ctx, target.ID, repo.TargetFailed, "codex: install: exit status 2", "")
	if _, err := s.dbm.ops.RetryTarget(ctx, target.ID, "admin", "r3"); err != nil {
		t.Fatal(err)
	}
	s.dbm.ops.RequestSync(ctx, "PC-STORY", "admin", "r4")
	// alice has since left and been deleted; her name must still show.
	s.dbm.ops.Offboard(ctx, alice.ID, "admin", "r5")
	s.dbm.ops.Delete(ctx, alice.ID, "admin", "r6")

	page := dbGet(t, h, "/machines/detail?machine=PC-STORY", cookie)
	body := page.Body.String()
	for _, want := range []string{"PC-STORY", ">alice ", "账号已删除", ">bob<", "当前", "0.42.0", "失败", "codex: install: exit status 2",
		"待执行", "machine.bind", "machine.sync", "release.target_retry", rollout.ID} {
		if !strings.Contains(body, want) {
			t.Errorf("machine page lacks %q", want)
		}
	}
	if page.Code != 200 {
		t.Fatalf("status %d", page.Code)
	}
	// The overview links here.
	if ov := dbGet(t, h, "/overview", cookie); !strings.Contains(ov.Body.String(), `href="/machines/detail?machine=PC-STORY"`) {
		t.Fatal("the overview does not link to the machine page")
	}
	if rec := dbGet(t, h, "/machines/detail?machine=NOPE", cookie); rec.Code != 303 {
		t.Fatalf("unknown machine: %d", rec.Code)
	}
}

func TestUserDetailShowsTheEmployeesHistory(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	csrf := csrfFrom(t, s, cookie, "/users")
	dbPost(t, h, "/users/onboard", url.Values{"csrf": {csrf}, "windowsUser": {"carol"}, "email": {"t@example.com"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}, cookie)
	carol, _ := s.dbm.store.Employees().ByWindowsUser(ctx, "carol")
	s.dbm.ops.SetModels(ctx, carol.ID, []string{"glm-5"}, "admin", "r1")
	s.dbm.ops.BindMachine(ctx, "PC-C1", carol.ID, "desk 4", "admin", "r2")
	s.dbm.ops.UnbindMachine(ctx, "PC-C1", "admin", "r3")
	s.dbm.ops.BindMachine(ctx, "PC-C2", carol.ID, "", "admin", "r4")
	s.dbm.ops.Offboard(ctx, carol.ID, "admin", "r5")

	page := dbGet(t, h, "/users/detail?user=carol", cookie)
	body := page.Body.String()
	for _, want := range []string{"PC-C1", "PC-C2", "desk 4", "当前", "account.onboard", "account.models", "account.offboard",
		`href="/machines/detail?machine=PC-C2"`, "target_type=employee&target=" + carol.ID} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page lacks %q", want)
		}
	}
	if page.Code != 200 {
		t.Fatalf("status %d", page.Code)
	}
}

func TestTheMachinePageSetsTheMachinesVersion(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	store := s.dbm.store
	device, _ := store.Devices().EnsureByHostname(ctx, "PC-V")
	store.Devices().MarkSeen(ctx, device.ID, "1.3.0", time.Now())
	old, _ := store.Devices().EnsureByHostname(ctx, "PC-OLD")
	store.Devices().MarkSeen(ctx, old.ID, "1.2.14", time.Now())
	a, _ := s.dbm.ops.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductAgent, Version: "1.3.1",
		SHA256: strings.Repeat("d", 64), SizeBytes: 1, ObjectKey: "k", CreatedBy: "t"}, "t", "r")

	page := dbGet(t, h, "/machines/detail?machine=PC-V", cookie).Body.String()
	if !strings.Contains(page, "跟随全局") || !strings.Contains(page, `value="`+a.ID+`"`) {
		t.Fatal("the machine page should offer 跟随全局 and the library's versions")
	}
	if page := dbGet(t, h, "/machines/detail?machine=PC-OLD", cookie).Body.String(); !strings.Contains(page, "太旧") {
		t.Fatal("an agent that cannot read a target of its own must be told so, not offered a choice")
	}

	csrf := csrfFrom(t, s, cookie, "/machines/detail?machine=PC-V")
	rec := dbPost(t, h, "/machines/version", url.Values{"csrf": {csrf}, "machine": {"PC-V"}, "product": {"agent"}, "artifact": {a.ID}}, cookie)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") || !strings.Contains(loc, "/machines/detail?machine=PC-V") {
		t.Fatalf("pin: %s", loc)
	}
	if _, err := store.Releases().OpenTarget(ctx, device.ID, repo.ProductAgent); err != nil {
		t.Fatalf("pinning must open a target: %v", err)
	}
	if page := dbGet(t, h, "/machines/detail?machine=PC-V", cookie).Body.String(); !strings.Contains(page, "单独指定") {
		t.Fatal("the page should say the machine has a version of its own")
	}
	rec = dbPost(t, h, "/machines/version", url.Values{"csrf": {csrf}, "machine": {"PC-V"}, "product": {"agent"}, "artifact": {""}}, cookie)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("follow global: %s", loc)
	}
	if _, err := store.Releases().OpenTarget(ctx, device.ID, repo.ProductAgent); err == nil {
		t.Fatal("跟随全局 must drop the machine's own target")
	}
}
