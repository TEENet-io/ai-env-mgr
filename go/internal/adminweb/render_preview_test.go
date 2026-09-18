package adminweb

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/slsclient"
)

// TestRenderPreview writes every page to ADMINWEB_PREVIEW_DIR with fixture
// data, so the design can actually be looked at rather than reasoned about.
// It is skipped unless that variable is set.
func TestRenderPreview(t *testing.T) {
	dir := os.Getenv("ADMINWEB_PREVIEW_DIR")
	if dir == "" {
		t.Skip("set ADMINWEB_PREVIEW_DIR to write preview pages")
	}
	s, err := New(Options{Listen: "127.0.0.1:0", Bucket: "ai-collect-sg", Endpoint: "oss-ap-southeast-1.aliyuncs.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Relative to now, so the preview shows the states an operator would
	// actually see rather than whatever the fixed stamps happen to classify as.
	ago := func(d time.Duration) string {
		return time.Now().Add(-d).UTC().Format(time.RFC3339)
	}
	mk := func(name, user string, st admincore.MachineState) admincore.MachineState {
		st.Machine = name
		st.Binding = model.Binding{User: user, RestartCodex: st.Binding.RestartCodex}
		st.Bound = user != ""
		return st
	}
	machines := []admincore.MachineState{
		mk("hv8uqpity23nkc7", "peter", admincore.MachineState{UserMissing: true,
			Status: model.Status{LastSync: ago(50 * time.Minute), AgentVersion: "1.2.4",
				AppLockerMode: "Enforce"}}),
		mk("wuying-desk-0142", "work1", admincore.MachineState{
			Status: model.Status{LastSync: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				AgentVersion: "1.2.7", CodexVersion: "26.810.52044-b1",
				AppLockerMode: "Audit"}}),
		mk("wuying-desk-0143", "work2", admincore.MachineState{
			Status: model.Status{LastSync: ago(90 * time.Second),
				AgentVersion: "1.2.7", CodexVersion: "26.803.81509-b1",
				CodexState: agentcore.CodexFailed, AppLockerMode: "Enforce"}}),
		mk("wuying-desk-0144", "lena", admincore.MachineState{Stale: true,
			Status: model.Status{LastSync: ago(5 * 24 * time.Hour), AgentVersion: "1.2.3",
				AppLockerMode: "None"}}),
		mk("wuying-desk-0145", "", admincore.MachineState{Unbound: true,
			Status: model.Status{LastSync: ago(2 * time.Minute), AgentVersion: "1.2.4"}}),
		// The two states of a one-shot restart request: asked but not yet
		// picked up, and carried out.
		mk("wuying-desk-0149", "mei", admincore.MachineState{
			Binding: model.Binding{RestartCodex: "1a2b3c4d5e6f7080"},
			Status: model.Status{LastSync: ago(2 * time.Minute), AgentVersion: "1.2.7",
				AppLockerMode: "Enforce"}}),
		mk("wuying-desk-0150", "tan", admincore.MachineState{
			Binding: model.Binding{RestartCodex: "7b3c9d1e2f4a5b60"},
			Status: model.Status{LastSync: ago(3 * time.Minute), AgentVersion: "1.2.7",
				AppLockerMode: "Enforce", CodexRestartNonce: "7b3c9d1e2f4a5b60",
				CodexRestartAt: ago(4 * time.Minute), CodexRestartNote: "killed 1 process"}}),
		mk("wuying-desk-0146", "chen", admincore.MachineState{Missing: true}),
		mk("wuying-desk-0147", "zhao", admincore.MachineState{
			Status: model.Status{LastSync: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
				AgentVersion: "1.2.4", LastEvent: "suspend"}}),
		mk("wuying-desk-0148", "liu", admincore.MachineState{
			Status: model.Status{LastSync: time.Now().Add(-9 * time.Hour).UTC().Format(time.RFC3339),
				AgentVersion: "1.2.4", LastEvent: "stopped"}}),
	}
	policy := model.Policy{
		SyncIntervalMinutes: 15, BlockEnabled: true,
		BlockedDomains:     []string{"chat.openai.com", "gemini.google.com", "poe.com"},
		AppLockerMode:      model.AppLockerModeAudit,
		CollectEnabled:     true,
		CollectSince:       "2026-08-01",
		AgentUpdateVersion: "1.2.4",
		AgentUpdateSHA256:  "d4626f337aeb4e34a12f0e3c9fa1a559ab1377c3d4626f337aeb4e34a12f0e3c",
		CodexVersion:       "26.810.52044-b1", CodexRolloutPct: 100,
		UpdatedAt: "2026-08-16T09:03:50Z",
	}
	users := []model.UserEntry{
		{WindowsUser: "peter", CodexAccount: "peter@teenet.io", Enabled: true},
		{WindowsUser: "work1", CodexAccount: "work1@teenet.io", Enabled: true},
		{WindowsUser: "chen", Enabled: false},
	}
	accountRows := []accountRow{
		{WindowsUser: "peter", Name: "Peter", Department: "研发", Enabled: true, OnRoster: true,
			HasUser: true, HasToken: true, Models: []string{"grok-4.6", "glm-5"},
			Spend: 12.4, Budget: 20, BudgetResetAt: "2026-10-01T00:00:00Z",
			Quota:    litellm.Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4},
			Machines: []string{"hv8uqpity23nkc7"}},
		{WindowsUser: "work1", Name: "Work One", Department: "运营", Enabled: true, OnRoster: true,
			HasUser: true, HasToken: true, Models: []string{"deepseek-v3.2"},
			Spend: 18.7, Budget: 20, BudgetResetAt: "2026-10-01T00:00:00Z",
			Quota:    litellm.Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4},
			Machines: []string{"wuying-desk-0142"}},
		{WindowsUser: "chen", Enabled: false, HasToken: true, OnRoster: true, Flags: []accountFlag{flagDepartedToken}},
		{WindowsUser: "ghost", Enabled: false, HasToken: true, Flags: []accountFlag{flagDepartedToken}},
	}
	base := pageData{
		Bucket: "ai-collect-sg", Endpoint: "oss-ap-southeast-1.aliyuncs.com",
		CSRF: "preview", Machines: machines, Fleet: summariseFleet(machines),
		Users: users, Policy: &policy,
		Accounts: accountRows, Account: &accountRows[0], QuotaDefaults: admincore.DefaultQuota,
		Audit: []admincore.AuditEntry{
			{At: "2026-09-01T02:00:00Z", Action: admincore.AuditOnboard, User: "peter"},
			{At: "2026-09-05T08:30:00Z", Action: admincore.AuditQuota, User: "peter"},
		},
		Stats: []admincore.CollectStat{
			{User: "peter", Codex: 128, Total: 128, Latest: time.Now()},
			{User: "work1", Codex: 64, Total: 95, Latest: time.Now()},
		},
		Machine: "hv8uqpity23nkc7",
		Log: "2026-08-16T01:35:54Z sync ok (policy etag W/\"a1b2\")\n" +
			"2026-08-16T01:35:54Z credentials: nothing new\n" +
			"2026-08-16T01:35:55Z status uploaded",
		Job: &job{Kind: "codex", Version: "26.810.52044-b1", State: jobRunning,
			Step: "下载安装包", Started: time.Now().Add(-95 * time.Second),
			Done: 412 << 20, Total: 700 << 20},
		Fixed: true,

		// The log page never talks to SLS here: the handler turns query
		// results into this shape with pure functions, so the preview fills
		// one in directly and the design can be looked at without a project.
		SLS:  true,
		Logs: previewLogsPage(),

		// The overview's own gateway health. A failing hour rather than a
		// green one: the red state is the one whose layout has to be looked
		// at, and a preview of the happy path proves the least.
		Probe: probeFrom(
			[]slsclient.Log{{"oks": "52", "fails": "3"}},
			[]slsclient.Log{{"occurred_at": "2026-09-12T02:41:08.220Z", "message": "probe failed: 502 Bad Gateway"}},
		),
		// And the disclosure that the count above may be low, which is the
		// line that stops a partial tally from reading as the hour's total.
		ProbeIncomplete: true,

		GatewayURL:     "https://litellm.teenet.app",
		GatewayEnabled: true,
		GatewayModels: []litellm.Model{
			{Name: "grok-4.6", Info: litellm.ModelInfo{DisplayName: "Grok 4.6", ContextWindow: 256000, ReasoningLevels: []string{"low", "high"}}},
			{Name: "deepseek-v3.2", Info: litellm.ModelInfo{DisplayName: "DeepSeek V3.2", ContextWindow: 128000, ReasoningLevels: []string{"low", "high"}}},
			{Name: "glm-5", Info: litellm.ModelInfo{DisplayName: "智谱 GLM-5", ContextWindow: 128000, ReasoningLevels: []string{"low", "high"}}},
		},
	}
	for _, tplName := range []string{
		"login.html", "overview.html", "users.html", "user.html", "sites.html",
		"settings.html", "rollout.html", "log.html", "logs.html",
	} {
		d := base
		d.Nav = map[string]string{
			"overview.html": "overview", "users.html": "users", "user.html": "users",
			"sites.html":    "sites",
			"settings.html": "settings",
			"rollout.html":  "rollout", "log.html": "overview",
			"logs.html": "logs",
		}[tplName]
		f, err := os.Create(filepath.Join(dir, tplName))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.tpl.ExecuteTemplate(f, tplName, d); err != nil {
			t.Fatalf("%s: %v", tplName, err)
		}
		f.Close()
	}
	// The stylesheet, so the files open standalone.
	css, err := assetFS.ReadFile("assets/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "static"), 0o755)
	os.WriteFile(filepath.Join(dir, "static", "app.css"), css, 0o644)
	t.Logf("wrote preview pages to %s", dir)
}

// previewLogsPage is a plausible page of calls: a success, a failure with an
// error class, a cancellation and a call the gateway could not price, so
// every cell the template has a branch for is actually exercised.
func previewLogsPage() *logsPage {
	f := logFilter{Range: "7d", Page: 1}
	p := newLogsPage("windows-control-logs", f, []string{"chen", "peter", "work1"})
	p.Summary = summaryFrom([]slsclient.Log{
		{"calls": "1842", "failures": "23", "cancelled": "4", "cost": "37.9142"},
	})
	p.Probe = probeFrom(
		[]slsclient.Log{{"oks": "58", "fails": "2"}},
		[]slsclient.Log{{"occurred_at": "2026-09-12T02:41:08.220Z", "message": "probe failed: 502 Bad Gateway"}},
	)
	p.Rows = logRowsFrom([]slsclient.Log{
		{"occurred_at": "2026-09-12T03:12:44.118Z", "employee_id": "emp-peter", "status": "success",
			"model_group": "glm-5", "model": "zhipu/glm-5", "latency_ms": "1483.2",
			"total_tokens": "1520", "cost_usd": "0.000421", "cost_state": "estimated"},
		{"occurred_at": "2026-09-12T03:09:02.771Z", "employee_id": "emp-work1", "status": "failure",
			"model_group": "grok-4.6", "model": "xai/grok-4.6", "latency_ms": "812",
			"error_class": "rate_limit", "error_code": "429", "cost_usd": "0.0", "cost_state": "estimated"},
		{"occurred_at": "2026-09-12T02:58:30.004Z", "employee_id": "emp-peter", "status": "cancelled",
			"model_group": "deepseek-v3.2", "model": "deepseek/deepseek-v3.2", "latency_ms": "240",
			"cost_state": "unknown"},
		{"occurred_at": "2026-09-12T02:41:19.900Z", "employee_id": "emp-chen", "status": "success",
			"model_group": "glm-5", "model": "zhipu/glm-5", "latency_ms": "6210",
			"total_tokens": "48210", "cost_usd": "0.013877", "cost_state": "estimated"},
		// The two rows that are not an employee on the roster: a call on the
		// gateway's own key, and one billed to somebody who has left.
		{"occurred_at": "2026-09-12T02:30:11.500Z", "employee_id": "default_user_id", "status": "success",
			"model_group": "glm-5", "model": "zhipu/glm-5", "latency_ms": "990",
			"total_tokens": "410", "cost_usd": "0.000118", "cost_state": "estimated"},
		{"occurred_at": "2026-09-12T02:22:04.010Z", "employee_id": "emp-eventprobe", "status": "success",
			"model_group": "glm-5", "model": "zhipu/glm-5", "latency_ms": "1180",
			"total_tokens": "300", "cost_usd": "0.000090", "cost_state": "estimated"},
	}, rosterOf("chen", "peter", "work1"))
	p.ListOK = true
	// The page's own filter, so the preview shows what page 1 actually looks
	// like: a next link and no previous one.
	p.PrevURL, p.NextURL = f.pageLinks(logsPageSize)
	return p
}
