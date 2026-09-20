package adminweb

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestTargetRowStateNamesEveryStageWithoutGuessing(t *testing.T) {
	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	made := now.Add(-30 * time.Minute)
	artifact := repo.Artifact{Product: repo.ProductCodex, Version: "0.42.0"}
	pending := repo.Target{Status: repo.TargetPending, Generation: 2, CreatedAt: made, Product: repo.ProductCodex}
	recent := now.Add(-2 * time.Minute)
	old := now.Add(-3 * time.Hour)

	cases := []struct {
		name     string
		target   repo.Target
		report   *model.Status
		lastSync *time.Time
		label    string
	}{
		{"never reported", pending, nil, nil, "待执行(离线)"},
		{"online, not started", pending, &model.Status{CodexVersion: "0.41.0"}, &recent, "待执行(在线)"},
		{"offline for hours", pending, &model.Status{CodexVersion: "0.41.0"}, &old, "待执行(离线)"},
		{"deferred in use", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 2, CodexState: "deferred", CodexDeferReason: "in_use"}, &recent, "延后:正在使用"},
		{"deferred disk", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 2, CodexState: "deferred", CodexDeferReason: "disk"}, &recent, "延后:磁盘不足"},
		{"downloading", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 2, CodexState: "downloading"}, &recent, "下载中"},
		{"installing", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 2, CodexState: "installing"}, &recent, "安装中"},
		{"installed, not yet settled", pending, &model.Status{CodexVersion: "0.42.0"}, &recent, "待健康确认"},
		{"old generation's failure", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 1, CodexState: "failed"}, &recent, "待执行(在线)"},
		{"succeeded", repo.Target{Status: repo.TargetSucceeded, ReportedVersion: "0.42.0"}, nil, nil, "成功"},
		{"failed", repo.Target{Status: repo.TargetFailed, ResultNote: "codex: install: exit status 2"}, nil, nil, "失败"},
		{"excluded", repo.Target{Status: repo.TargetExcluded, ExcludeReason: "机器已停用"}, nil, nil, "已排除"},
		{"cancelled", repo.Target{Status: repo.TargetCancelled}, nil, nil, "已取消"},
		{"superseded", repo.Target{Status: repo.TargetSuperseded}, nil, nil, "已取代"},
	}
	for _, c := range cases {
		label, _ := targetRowState(c.target, artifact, c.report, c.lastSync, now)
		if label != c.label {
			t.Errorf("%s: label = %q, want %q", c.name, label, c.label)
		}
	}
}

func TestRolloutSummaryKeepsExcludedApartFromSuccess(t *testing.T) {
	targets := []repo.Target{
		{DeviceID: "a", Generation: 1, Status: repo.TargetSucceeded}, {DeviceID: "b", Generation: 1, Status: repo.TargetSucceeded},
		{DeviceID: "c", Generation: 1, Status: repo.TargetExcluded}, {DeviceID: "d", Generation: 1, Status: repo.TargetFailed},
		{DeviceID: "e", Generation: 1, Status: repo.TargetPending},
	}
	sum := summariseTargets(targets)
	if sum.Succeeded != 2 || sum.Excluded != 1 || sum.Failed != 1 || sum.Pending != 1 || sum.Total != 5 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Complete() {
		t.Fatal("a rollout with a pending machine is not complete")
	}
	sum = summariseTargets([]repo.Target{{DeviceID: "a", Generation: 1, Status: repo.TargetSucceeded}, {DeviceID: "c", Generation: 1, Status: repo.TargetExcluded}})
	if !sum.Complete() {
		t.Fatal("all machines succeeded or were explicitly excluded: complete")
	}
}

func TestRolloutSummaryCountsEachMachinesLatestAttempt(t *testing.T) {
	// d failed on generation 1 and succeeded on the retry: one success.
	sum := summariseTargets([]repo.Target{
		{DeviceID: "d", Generation: 1, Status: repo.TargetFailed},
		{DeviceID: "d", Generation: 2, Status: repo.TargetSucceeded},
	})
	if sum.Total != 1 || sum.Succeeded != 1 || sum.Failed != 0 || !sum.Complete() {
		t.Fatalf("retry summary = %+v", sum)
	}
	// Every target taken over by a newer rollout: history, not completion.
	sum = summariseTargets([]repo.Target{
		{DeviceID: "a", Generation: 1, Status: repo.TargetSuperseded},
		{DeviceID: "b", Generation: 1, Status: repo.TargetSuperseded},
	})
	if sum.Complete() || sum.Superseded != 2 {
		t.Fatalf("superseded summary = %+v", sum)
	}
}

func TestChoosingOneMachineLeavesTheOtherAlone(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	store := s.dbm.store
	a, _ := s.dbm.ops.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.42.0",
		SHA256: strings.Repeat("c", 64), SizeBytes: 1, ObjectKey: "k", CreatedBy: "t"}, "t", "r")
	pcA, _ := store.Devices().EnsureByHostname(ctx, "PC-A")
	pcB, _ := store.Devices().EnsureByHostname(ctx, "PC-B")
	pcOld, _ := store.Devices().EnsureByHostname(ctx, "PC-OLD")
	store.Devices().MarkSeen(ctx, pcA.ID, "1.2.16", time.Now())
	store.Devices().MarkSeen(ctx, pcB.ID, "1.2.16", time.Now())
	store.Devices().MarkSeen(ctx, pcOld.ID, "1.2.15", time.Now())

	page := dbGet(t, h, "/rollouts/new?artifact="+a.ID, cookie)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "PC-A") || !strings.Contains(body, "PC-B") {
		t.Fatalf("new rollout page: %d", page.Code)
	}
	if !strings.Contains(body, `value="`+pcA.ID+`"`) || strings.Contains(body, `value="`+pcOld.ID+`"`) {
		t.Fatal("machines that cannot take a target must not be selectable")
	}
	csrf := csrfFrom(t, s, cookie, "/rollouts/new?artifact="+a.ID)
	rec := dbPost(t, h, "/rollouts/create", url.Values{"csrf": {csrf}, "product": {"codex"}, "artifact": {a.ID},
		"device": {pcA.ID}, "note": {"试发"}}, cookie)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("create: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := store.Releases().OpenTarget(ctx, pcA.ID, repo.ProductCodex); err != nil {
		t.Fatalf("PC-A: %v", err)
	}
	if _, err := store.Releases().OpenTarget(ctx, pcB.ID, repo.ProductCodex); err == nil {
		t.Fatal("PC-B was not chosen and must have no target")
	}
	rollouts, _ := store.Releases().ListRollouts(ctx, 1)
	detail := dbGet(t, h, "/rollouts/detail?id="+rollouts[0].ID, cookie)
	body = detail.Body.String()
	if detail.Code != 200 || !strings.Contains(body, "PC-A") || strings.Contains(body, "PC-B") || !strings.Contains(body, "待执行") {
		t.Fatalf("detail page: %d", detail.Code)
	}
	list := dbGet(t, h, "/rollouts", cookie)
	if list.Code != 200 || !strings.Contains(list.Body.String(), "0.42.0") {
		t.Fatalf("rollouts list: %d", list.Code)
	}

	// Pause, then exclude the one machine with a reason: the rollout is then
	// complete with nothing succeeded.
	csrf = csrfFrom(t, s, cookie, "/rollouts/detail?id="+rollouts[0].ID)
	if rec := dbPost(t, h, "/rollouts/pause", url.Values{"csrf": {csrf}, "id": {rollouts[0].ID}}, cookie); strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("pause: %s", rec.Header().Get("Location"))
	}
	target, _ := store.Releases().OpenTarget(ctx, pcA.ID, repo.ProductCodex)
	if rec := dbPost(t, h, "/rollouts/exclude", url.Values{"csrf": {csrf}, "id": {rollouts[0].ID}, "target": {target.ID}}, cookie); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("excluding without a reason must be refused")
	}
	dbPost(t, h, "/rollouts/exclude", url.Values{"csrf": {csrf}, "id": {rollouts[0].ID}, "target": {target.ID}, "reason": {"机器已停用"}}, cookie)
	detail = dbGet(t, h, "/rollouts/detail?id="+rollouts[0].ID, cookie)
	if !strings.Contains(detail.Body.String(), "已排除") || !strings.Contains(detail.Body.String(), "机器已停用") {
		t.Fatal("the exclusion and its reason must show on the page")
	}
}
