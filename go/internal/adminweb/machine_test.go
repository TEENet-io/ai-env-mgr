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
	for _, want := range []string{"PC-STORY", ">alice ", "帳號已刪除", ">bob<", "當前", "0.42.0", "失敗", "codex: install: exit status 2",
		"待執行", "machine.bind", "machine.sync", "release.target_retry", rollout.ID} {
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
	for _, want := range []string{"PC-C1", "PC-C2", "desk 4", "當前", "account.onboard", "account.models", "account.offboard",
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
	if !strings.Contains(page, "跟隨全局") || !strings.Contains(page, `value="`+a.ID+`"`) {
		t.Fatal("the machine page should offer 跟隨全局 and the library's versions")
	}
	if page := dbGet(t, h, "/machines/detail?machine=PC-OLD", cookie).Body.String(); !strings.Contains(page, "太舊") {
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
	if page := dbGet(t, h, "/machines/detail?machine=PC-V", cookie).Body.String(); !strings.Contains(page, "單獨指定") {
		t.Fatal("the page should say the machine has a version of its own")
	}
	rec = dbPost(t, h, "/machines/version", url.Values{"csrf": {csrf}, "machine": {"PC-V"}, "product": {"agent"}, "artifact": {""}}, cookie)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("follow global: %s", loc)
	}
	if _, err := store.Releases().OpenTarget(ctx, device.ID, repo.ProductAgent); err == nil {
		t.Fatal("跟隨全局 must drop the machine's own target")
	}
}

// A machine gets a short name people can tell apart (BPO1). The bind form
// picks from every machine, the assigned ones too, so an operator can take
// one over; a ticket finds a machine by that name.
func TestAMachineGetsANameAndIsPickedFromTheList(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	store := s.dbm.store
	csrf := csrfFrom(t, s, cookie, "/users")
	dbPost(t, h, "/users/onboard", url.Values{"csrf": {csrf}, "windowsUser": {"alice"}, "email": {"t@example.com"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}, cookie)
	one, _ := store.Devices().EnsureByHostname(ctx, "DESKTOP-7F3KQ")
	store.Devices().EnsureByHostname(ctx, "DESKTOP-9ZX2M")
	alice, _ := store.Employees().ByWindowsUser(ctx, "alice")
	if _, err := s.dbm.ops.BindMachine(ctx, "DESKTOP-7F3KQ", alice.ID, "", "admin", "r1"); err != nil {
		t.Fatal(err)
	}

	label := func(machine, label, note string) string {
		rec := dbPost(t, h, "/machines/label", url.Values{"csrf": {csrfFrom(t, s, cookie, "/overview")}, "machine": {machine}, "label": {label}, "note": {note}}, cookie)
		return rec.Header().Get("Location")
	}
	if loc := label("DESKTOP-7F3KQ", "BPO1", "前台"); strings.Contains(loc, "err=") {
		t.Fatalf("set label: %q", loc)
	}
	if d, _ := store.Devices().ByID(ctx, one.ID); d.Label != "BPO1" || d.Note != "前台" || d.Hostname != "DESKTOP-7F3KQ" {
		t.Fatalf("after labelling: %+v", d)
	}
	for _, bad := range []struct{ label, why string }{
		{"bpo1", "a label another machine has, in another case"},
		{"DESKTOP-7F3KQ", "another machine's hostname"},
		{"BPO 2", "a space"},
	} {
		if loc := label("DESKTOP-9ZX2M", bad.label, ""); !strings.Contains(loc, "err=") {
			t.Errorf("%s was accepted: %q", bad.why, loc)
		}
	}
	if loc := label("DESKTOP-9ZX2M", "BPO2", ""); strings.Contains(loc, "err=") {
		t.Fatalf("second label: %q", loc)
	}

	page := dbGet(t, h, "/overview", cookie).Body.String()
	for _, want := range []string{"<strong>BPO1</strong>", "前台",
		`<option value="DESKTOP-7F3KQ">BPO1 · DESKTOP-7F3KQ · 現在綁給 alice</option>`,
		`<option value="DESKTOP-9ZX2M">BPO2 · DESKTOP-9ZX2M · 未分配</option>`} {
		if !strings.Contains(page, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	bind := page[strings.Index(page, `action="/machines/bind"`):]
	bind = bind[:strings.Index(bind, "</form>")]
	if strings.Contains(bind, `<input name="machine"`) {
		t.Error("the bind form still takes a typed hostname")
	}

	// A ticket is linked to the machine by its short name.
	contact, err := s.dbm.db.Tickets().CreateContactWithRole(ctx, "c@example.com", "C", "hash", "employee")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.dbm.db.Tickets().Create(ctx, contact.ID, "label-key", "bug", "打不開", "打不開", "normal", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.dbm.db.Tickets().Get(ctx, ticket.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	rootName, _ := knownAdmin(t, s)
	root, err := store.Admins().ByUsername(ctx, rootName)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.dbm.db.Tickets().LinkSubject(ctx, ticket.ID, "device", "bpo2", root.ID, current.Version); err != nil {
		t.Fatalf("link by label: %v", err)
	}
	if got, _ := s.dbm.db.Tickets().Get(ctx, ticket.ID, ""); got.DeviceHostname != "DESKTOP-9ZX2M" {
		t.Errorf("ticket linked to %q", got.DeviceHostname)
	}
}

func TestAViewerCannotNameAMachine(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	ctx := t.Context()
	device, _ := s.dbm.store.Devices().EnsureByHostname(ctx, "PC-V")
	_, password, err := s.dbm.auth.CreateAccount(ctx, "eve", "", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	viewer := signInAs(t, s, "eve", password)
	if page := dbGet(t, h, "/machines/detail?machine=PC-V", viewer).Body.String(); strings.Contains(page, `action="/machines/label"`) {
		t.Error("a viewer is offered the label form")
	}
	dbPost(t, h, "/machines/label", url.Values{"csrf": {csrfFrom(t, s, viewer, "/settings")}, "machine": {"PC-V"}, "label": {"BPO9"}}, viewer)
	if d, _ := s.dbm.store.Devices().ByID(ctx, device.ID); d.Label != "" {
		t.Errorf("a viewer named a machine %q", d.Label)
	}
}
