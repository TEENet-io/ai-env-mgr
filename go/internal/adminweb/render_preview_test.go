package adminweb

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
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
		st.Binding = model.Binding{User: user}
		st.Bound = user != ""
		return st
	}
	machines := []admincore.MachineState{
		mk("hv8uqpity23nkc7", "peter", admincore.MachineState{UserMissing: true,
			Status: model.Status{LastSync: ago(50 * time.Minute), AgentVersion: "1.2.4"}}),
		mk("wuying-desk-0142", "work1", admincore.MachineState{
			Status: model.Status{LastSync: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				AgentVersion: "1.2.7", CodexVersion: "26.810.52044-b1"}}),
		mk("wuying-desk-0143", "work2", admincore.MachineState{
			Status: model.Status{LastSync: ago(90 * time.Second),
				AgentVersion: "1.2.7", CodexVersion: "26.803.81509-b1",
				CodexState: agentcore.CodexFailed}}),
		mk("wuying-desk-0144", "lena", admincore.MachineState{Stale: true,
			Status: model.Status{LastSync: ago(5 * 24 * time.Hour), AgentVersion: "1.2.3"}}),
		mk("wuying-desk-0145", "", admincore.MachineState{Unbound: true,
			Status: model.Status{LastSync: ago(2 * time.Minute), AgentVersion: "1.2.4"}}),
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
		CollectEnabled:     true,
		CollectSince:       "2026-08-01",
		AgentUpdateVersion: "1.2.4",
		AgentUpdateSHA256:  "d4626f337aeb4e34a12f0e3c9fa1a559ab1377c3d4626f337aeb4e34a12f0e3c",
		CodexVersion:       "26.810.52044-b1", CodexRolloutPct: 100,
		UpdatedAt: "2026-08-16T09:03:50Z",
	}
	users := []model.UserEntry{
		{WindowsUser: "peter", CodexAccount: "peter@teenet.io", Enabled: true},
		{WindowsUser: "work1", CodexAccount: "work1@teenet.io", ClaudeAccount: "work1@teenet.io", Enabled: true},
		{WindowsUser: "chen", Enabled: false},
	}
	base := pageData{
		Bucket: "ai-collect-sg", Endpoint: "oss-ap-southeast-1.aliyuncs.com",
		CSRF: "preview", Machines: machines, Fleet: summariseFleet(machines),
		Users: users, Policy: &policy,
		Stats: []admincore.CollectStat{
			{User: "peter", Codex: 128, Claude: 0, Total: 128, Latest: time.Now()},
			{User: "work1", Codex: 64, Claude: 31, Total: 95, Latest: time.Now()},
		},
		Files:   []admincore.StagedFile{{Name: "agent-1.2.4.exe"}, {Name: "codex-setup.exe"}},
		Machine: "hv8uqpity23nkc7",
		Log: "2026-08-16T01:35:54Z sync ok (policy etag W/\"a1b2\")\n" +
			"2026-08-16T01:35:54Z credentials: nothing new\n" +
			"2026-08-16T01:35:55Z status uploaded",
		Link:        "https://ai-collect-sg.oss-ap-southeast-1.aliyuncs.com/admin/files/agent-1.2.4.exe?Expires=1786717938&Signature=abc%3D",
		LinkName:    "agent-1.2.4.exe",
		AuthURL:     "https://auth.openai.com/oauth/authorize?client_id=app_EMoamEEZ73f0CkXaXp7hrann&code_challenge=8q3n…&state=7f2a9c",
		PendingUser: "peter", PendingTool: "codex",
		Job: &job{Kind: "codex", Version: "26.810.52044-b1", State: jobRunning,
			Step: "下载安装包", Started: time.Now().Add(-95 * time.Second),
			Done: 412 << 20, Total: 700 << 20},
		Fixed: true,
	}
	for _, name := range []string{
		"login.html", "machines.html", "users.html", "employee-login", "sites.html",
		"settings.html", "files.html", "rollout.html", "policy.html", "log.html",
	} {
		tplName := name
		if name == "employee-login" {
			tplName = "employeelogin.html"
		}
		d := base
		d.Nav = map[string]string{
			"machines.html": "machines", "users.html": "users",
			"employeelogin.html": "employee-login", "sites.html": "sites",
			"settings.html": "settings", "files.html": "files",
			"rollout.html": "rollout", "policy.html": "policy", "log.html": "machines",
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
