package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentapi"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/managedskills"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// Linked as "1" only in the downloadable testing package.
var skillsTestBuild string

func checkSkillsTestBuildArgs(args []string) error {
	if len(args) > 0 && (args[0] == "skills-test" || args[0] == "version") {
		return nil
	}
	return errors.New("this test package only supports: agent-skills-test.exe skills-test [--cleanup]; it cannot install or control the normal Agent service")
}

// skillsTestApplier deliberately has no registry, credential or restart side
// effects. Cache refreshes are owned by the real managedskills.Manager only.
type skillsTestApplier struct{}

func (skillsTestApplier) ApplyPolicy(model.Policy) error        { return nil }
func (skillsTestApplier) ApplyAppLocker([]string, string) error { return nil }
func (skillsTestApplier) RemoveCreds(string) (int, error)       { return 0, nil }
func (skillsTestApplier) DeployCreds(string, model.CredentialSet) (agentcore.Delivery, error) {
	return agentcore.Delivery{}, errors.New("credential delivery is disabled in skill test mode")
}
func (skillsTestApplier) StopCodex(string) (int, error) { return 0, nil }

func checkSkillsTestConsole(raw string) error {
	if err := config.CheckConsoleURL(raw); err != nil {
		return err
	}
	u, _ := url.Parse(raw)
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if host == "localhost" || ip != nil && ip.IsLoopback() {
		return nil
	}
	if u.Scheme == "https" && strings.HasSuffix(host, ".trycloudflare.com") {
		return nil
	}
	return errors.New("skill test mode only connects to a loopback or temporary trycloudflare.com test console")
}

func newSkillsTestSyncer(source agentcore.Source, machine agentcore.Machine, manager *managedskills.Manager, dir string) *agentcore.Syncer {
	return &agentcore.Syncer{Source: source, Machine: machine, Applier: skillsTestApplier{}, Skills: manager, StateDir: dir, Version: version + "-skills-only", FallbackInterval: 5}
}

func cmdSkillsTest(args []string) error {
	if runtime.GOOS != "windows" {
		return errors.New("skill test mode runs on Windows")
	}
	flags := flag.NewFlagSet("skills-test", flag.ContinueOnError)
	console := flags.String("console", consoleURL, "isolated test console origin")
	cleanup := flags.Bool("cleanup", false, "revoke only this test agent's managed skills")
	once := flags.Bool("once", false, "perform one test synchronization")
	interval := flags.Duration("interval", 5*time.Second, "test poll interval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected skill test argument")
	}
	if *interval < time.Second || *interval > time.Minute {
		return errors.New("interval must be between 1s and 1m")
	}
	if !*cleanup {
		if err := checkSkillsTestConsole(*console); err != nil {
			return err
		}
	}

	dir := filepath.Join(stateDir(), "skills-test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := managedskills.ProtectPrivate(dir); err != nil {
		return fmt.Errorf("protect test state (run as Administrator): %w", err)
	}
	manager := newManagedSkillsManager()
	manager.StateDir = dir
	if *cleanup {
		_, err := manager.Sync(model.ManagedSkills{})
		return err
	}
	machine, err := newLocalMachine()
	if err != nil {
		return err
	}
	fmt.Printf("SKILL TEST MODE: console=%s machine=%s state=%s\n", *console, machine.Name(), dir)
	fmt.Println("Only company skills are synchronized. Existing credentials, policies, applications and the installed Agent service are preserved.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var syncer *agentcore.Syncer
	var source *agentcore.APISource
	for {
		if source == nil {
			source = connectConsole(*console, machine.Name(), dir)
			if source != nil {
				syncer = newSkillsTestSyncer(source, machine, manager, dir)
			}
		}
		if source != nil {
			st, err := syncer.RunOnce()
			if err != nil {
				fmt.Println("sync:", err)
			} else if len(st.Errors) > 0 {
				fmt.Println("sync:", strings.Join(st.Errors, "; "))
			} else if st.ManagedSkills != nil {
				fmt.Printf("%s user=%s skills=%d state=%s\n", time.Now().Format("15:04:05"), st.ManagedSkills.User, len(st.ManagedSkills.Skills), st.ManagedSkills.State)
			} else {
				fmt.Println("test console did not provide a skill snapshot")
			}
			if *once {
				if err != nil {
					return err
				}
				if len(st.Errors) > 0 {
					return errors.New(strings.Join(st.Errors, "; "))
				}
				if st.ManagedSkills == nil {
					return errors.New("no skill snapshot received")
				}
				return nil
			}
			if source.NeedsEnrol() {
				if err := agentapi.ForgetToken(dir); err != nil {
					return err
				}
				source = nil
			}
		} else if *once {
			return errors.New("test console enrollment failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}
