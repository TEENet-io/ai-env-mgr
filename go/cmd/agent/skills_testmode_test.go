package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/agentapi"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/managedskills"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func TestSkillsTestPackageCannotInstallOrControlAgentService(t *testing.T) {
	for _, args := range [][]string{nil, {"setup"}, {"install"}, {"uninstall"}, {"start"}, {"stop"}, {"run"}, {"sync"}, {"prepare-image"}, {"--update-helper"}} {
		if err := checkSkillsTestBuildArgs(args); err == nil {
			t.Fatalf("test package accepted %v", args)
		}
	}
	for _, args := range [][]string{{"version"}, {"skills-test"}, {"skills-test", "--cleanup"}} {
		if err := checkSkillsTestBuildArgs(args); err != nil {
			t.Fatalf("test command rejected: %v", err)
		}
	}
}

func TestSkillsTestModeRejectsProductionConsole(t *testing.T) {
	for _, origin := range []string{"https://windows-control.teenet.app", "https://example.com", "http://10.0.0.1:18864", "https://demo.trycloudflare.com.example.com", "https://demo.trycloudflare.com/agent", "https://user:secret@demo.trycloudflare.com"} {
		if err := checkSkillsTestConsole(origin); err == nil {
			t.Fatalf("accepted non-test origin %s", origin)
		}
	}
	for _, origin := range []string{"http://127.0.0.1:18864", "http://localhost:18864", "https://demo.trycloudflare.com"} {
		if err := checkSkillsTestConsole(origin); err != nil {
			t.Fatalf("test origin %s: %v", origin, err)
		}
	}
}

type skillsTestMachine struct{ profile string }

func (m skillsTestMachine) Name() string             { return "SKILL-TEST-PC" }
func (m skillsTestMachine) LocalUsers() []string     { return []string{"work-test"} }
func (m skillsTestMachine) ProfileDir(string) string { return m.profile }

func TestSkillsTestCyclesKeepExistingLoginWhenUnboundRevokedOrIssuedCredentials(t *testing.T) {
	profile := t.TempDir()
	path := filepath.Join(profile, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing-login"), 0600); err != nil {
		t.Fatal(err)
	}
	bound, issueCredentials := false, false
	var mu sync.Mutex
	cycle := 0
	packed, err := creds.Pack(model.CredentialSet{model.PathCodexAuth: []byte("replacement")})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/agent/v1/config":
			cycle++
			cfg := deviceconfig.Config{ETag: strings.Repeat("e", cycle), HasPolicy: true, Policy: model.DefaultPolicy(), HasBinding: bound, Bound: bound, Skills: &model.ManagedSkills{}}
			if bound {
				cfg.Binding.User = "work-test"
				cfg.Skills.User = "work-test"
			}
			_ = json.NewEncoder(w).Encode(cfg)
		case "/agent/v1/credentials":
			if issueCredentials {
				_, _ = w.Write(packed)
			} else {
				http.NotFound(w, r)
			}
		case "/agent/v1/status":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected non-skill request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	source := agentcore.NewAPISource(&agentapi.Client{BaseURL: server.URL, HTTP: server.Client()})
	testDir := t.TempDir()
	manager := &managedskills.Manager{Root: filepath.Join(t.TempDir(), "Codex"), StateDir: testDir, Protect: func(string) error { return nil }, ProtectState: func(string) error { return nil }, Stop: func(string) error { t.Error("credential change stopped Codex"); return nil }}
	syncer := newSkillsTestSyncer(source, skillsTestMachine{profile}, manager, testDir)
	for _, step := range []struct{ bound, issued bool }{{false, false}, {true, false}, {true, true}} {
		mu.Lock()
		bound, issueCredentials = step.bound, step.issued
		mu.Unlock()
		st, err := syncer.RunOnce()
		if err != nil {
			t.Fatal(err)
		}
		if st.ManagedSkills == nil || st.ManagedSkills.State != "applied" {
			t.Fatalf("skill path did not run: %+v", st)
		}
		if issueCredentials && !strings.Contains(strings.Join(st.Errors, ";"), "credential delivery is disabled") {
			t.Fatalf("unexpected credential bundle was not rejected: %+v", st.Errors)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "existing-login" {
			t.Fatalf("test cycle changed existing login: %s %v", data, err)
		}
	}
}

func TestSkillsTestModePreservesExistingCredentialsAndDisablesOtherJobs(t *testing.T) {
	profile := t.TempDir()
	path := filepath.Join(profile, "auth.json")
	if err := os.WriteFile(path, []byte("existing-model-login"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newSkillsTestSyncer(nil, nil, nil, t.TempDir())
	if s.Collector != nil || s.Updater != nil || s.Codex != nil || s.Applications != nil {
		t.Fatal("test mode enabled a non-skill job")
	}
	if _, ok := s.Applier.(skillsTestApplier); !ok {
		t.Fatal("test mode uses the normal policy/credential applier")
	}
	if n, err := s.Applier.RemoveCreds(profile); err != nil || n != 0 {
		t.Fatalf("credential removal was enabled: %d %v", n, err)
	}
	if _, err := s.Applier.DeployCreds(profile, model.CredentialSet{model.PathCodexAuth: []byte("replacement")}); err == nil {
		t.Fatal("test mode accepted credential delivery")
	}
	if err := s.Applier.ApplyPolicy(model.DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := s.Applier.ApplyAppLocker([]string{`C:\test`}, "enforce"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "existing-model-login" {
		t.Fatalf("existing credentials changed: %s %v", data, err)
	}
}
