package ops

import (
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestPolicyEditsAreValidatedAndVersioned(t *testing.T) {
	svc, store, ctx := newService(t)

	// Nothing published yet: edits start from the built-in default.
	p, err := svc.MutateDomains(ctx, []string{" OpenAI.com ", "chatgpt.com", "example.org"}, []string{"claude.ai"}, "zhang", "")
	if err != nil {
		t.Fatalf("domains: %v", err)
	}
	if !p.BlockEnabled {
		t.Error("the default block switch was lost")
	}
	if strings.Join(p.BlockedDomains, ",") != "anthropic.com,chatgpt.com,example.org,openai.com" {
		t.Errorf("domains = %v, want normalised, deduplicated, sorted, with claude.ai removed", p.BlockedDomains)
	}

	if _, err := svc.MutateAppLockerAllowPaths(ctx, []string{`C:\Windows\Temp\*`}, nil, "zhang", ""); err == nil {
		t.Error("a user-writable Windows directory was accepted as an allow path")
	}
	p, err = svc.MutateAppLockerAllowPaths(ctx, []string{`C:\Tools\Codex\*`}, nil, "zhang", "")
	if err != nil {
		t.Fatalf("allow paths: %v", err)
	}
	if len(p.AppLockerAllowPaths) != 1 {
		t.Errorf("allow paths = %v", p.AppLockerAllowPaths)
	}

	if _, err := svc.SetAppLockerMode(ctx, "block-everything", "zhang", ""); err == nil {
		t.Error("a mode no agent understands was accepted")
	}
	if _, err := svc.SetAppLockerMode(ctx, "audit", "zhang", ""); err != nil {
		t.Fatalf("mode: %v", err)
	}

	// A bad interval can strand a machine; it is clamped, never refused.
	p, err = svc.SetSyncInterval(ctx, 100000, "zhang", "")
	if err != nil {
		t.Fatalf("interval: %v", err)
	}
	if p.SyncIntervalMinutes != 1440 {
		t.Errorf("interval = %d, want clamped to 1440", p.SyncIntervalMinutes)
	}
	if _, err := svc.SetBlockEnabled(ctx, false, "zhang", ""); err != nil {
		t.Fatalf("block: %v", err)
	}
	since := "2026-09-01"
	if _, err := svc.SetCollect(ctx, true, &since, nil, "zhang", ""); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if _, err := svc.SetAgentUpdate(ctx, "1.3.0", "short", "zhang", ""); err == nil {
		t.Error("an agent update without a proper SHA-256 was accepted")
	}
	sha := strings.Repeat("ab", 32)
	if _, err := svc.SetAgentUpdate(ctx, "1.3.0", sha, "zhang", ""); err != nil {
		t.Fatalf("agent update: %v", err)
	}
	p, err = svc.SetCodexUpdate(ctx, "26.9.1", sha, "agent_workdir/_codex/26.9.1.exe", "zhang", "")
	if err != nil {
		t.Fatalf("codex update: %v", err)
	}
	if p.CodexRolloutPct != 100 {
		t.Error("agents 1.2.5-1.2.7 would skip an update with no rollout percentage")
	}

	// Every edit is its own version, each carries every earlier edit, and the
	// export for it is queued.
	current, _, err := svc.CurrentPolicy(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current.AgentUpdateVersion != "1.3.0" || current.AppLockerMode != "audit" || current.BlockEnabled ||
		!current.CollectEnabled || current.CollectSince != "2026-09-01" || len(current.BlockedDomains) != 4 {
		t.Errorf("current policy lost an edit: %+v", current)
	}
	versions, err := store.Policies().List(ctx, 0)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 8 {
		t.Errorf("%d versions after 8 edits", len(versions))
	}
	if _, err := svc.SetAgentUpdate(ctx, "", "", "zhang", ""); err != nil {
		t.Fatalf("cancel agent update: %v", err)
	}
	history, err := store.Audit().Recent(ctx, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(history) != 9 || history[0].Action != ActionPublishPolicy {
		t.Errorf("audit = %v", actions(history))
	}
}

func TestForgetMachineRetiresItEverywhere(t *testing.T) {
	svc, store, ctx := newService(t)
	employee, err := svc.Onboard(ctx, OnboardSpec{WindowsUser: "work1", Quota: testQuota(), Actor: "zhang"})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if _, err := svc.BindMachine(ctx, "desktop-01", employee.ID, "", "zhang", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	finish(t, ctx, store)

	if err := svc.ForgetMachine(ctx, "DESKTOP-01", "zhang", ""); err != nil {
		t.Fatalf("forget: %v", err)
	}
	device, err := store.Devices().ByHostname(ctx, "desktop-01")
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	if device.Status != repo.DeviceRevoked {
		t.Errorf("device = %+v, want revoked", device)
	}
	if _, err := store.Bindings().Open(ctx, device.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Error("the machine is still bound")
	}
	// Its objects and the former holder's files are both rewritten.
	var deviceExport, employeeExport bool
	for _, task := range openTasks(t, ctx, store) {
		if task.Kind != repo.TaskOSSExport {
			continue
		}
		if task.DeviceID == device.ID {
			deviceExport = true
		}
		if task.EmployeeID == employee.ID {
			employeeExport = true
		}
	}
	if !deviceExport || !employeeExport {
		t.Errorf("exports queued: device=%v employee=%v, want both", deviceExport, employeeExport)
	}
	if err := svc.ForgetMachine(ctx, "nobody-01", "zhang", ""); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("forgetting an unknown machine: %v", err)
	}
}

func TestQuotaDefaultsFallBackOnlyWhenNothingIsStored(t *testing.T) {
	svc, _, ctx := newService(t)

	q, version, err := svc.QuotaDefaults(ctx)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if version != 0 || q != DefaultQuota {
		t.Errorf("defaults = %+v v%d, want the built-in at version 0", q, version)
	}

	if err := svc.SetQuotaDefaults(ctx, repo.Quota{MonthlyBudget: "0", RPM: 1, TPM: 1, Parallel: 1}, 0, "zhang", ""); err == nil {
		t.Error("a zero default budget was accepted")
	}
	if err := svc.SetQuotaDefaults(ctx, repo.Quota{MonthlyBudget: "35.5", RPM: 60, TPM: 500000, Parallel: 4}, 0, "zhang", ""); err != nil {
		t.Fatalf("set: %v", err)
	}
	q, version, err = svc.QuotaDefaults(ctx)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if q.MonthlyBudget != "35.5" || q.TPM != 500000 || version != 1 {
		t.Errorf("defaults = %+v v%d", q, version)
	}
	// A stale save is refused, not silently applied over the other one.
	if err := svc.SetQuotaDefaults(ctx, repo.Quota{MonthlyBudget: "40", RPM: 60, TPM: 500000, Parallel: 4}, 0, "li", ""); !errors.Is(err, repo.ErrConflict) {
		t.Errorf("stale save: %v", err)
	}
}
