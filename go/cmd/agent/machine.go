package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/policy"
	"github.com/TEENet-io/ai-env-mgr/internal/status"
)

// systemProfilePrefixes match profile directories created by tooling rather
// than by a person.
//
// Codex CLI creates CodexSandboxOffline/CodexSandboxOnline on Windows for its
// own sandboxing. Without this they show up as "unexpected accounts" on every
// machine on every sync -- and a warning that always fires is one nobody
// reads, which is exactly how a genuinely unexpected account would slip past.
//
// Matching by prefix rather than exact name means an account deliberately
// named to hide here would also be skipped. That needs local administrator
// rights to create, which is already game over by other routes, so the
// trade-off favours keeping the signal clean.
var systemProfilePrefixes = []string{"codexsandbox"}

// systemProfiles are the directories under the users root that belong to
// Windows itself rather than to a person.
var systemProfiles = map[string]bool{
	"public":             true,
	"default":            true,
	"default user":       true,
	"all users":          true,
	"wdagutilityaccount": true,
	"defaultapppool":     true,
	"systemprofile":      true,
	"localservice":       true,
	"networkservice":     true,
}

// localMachine reports the facts the sync logic needs about this host.
type localMachine struct {
	name string
}

// newLocalMachine resolves the hostname up front and fails loudly if it
// cannot: the hostname is this machine's identity in the store, and falling
// back to a placeholder would make several machines collide on one key.
func newLocalMachine() (*localMachine, error) {
	name, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read hostname: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("hostname is empty")
	}
	return &localMachine{name: name}, nil
}

func (m *localMachine) Name() string { return m.name }

// isSystemProfile reports whether a profile directory belongs to Windows or
// to tooling rather than to a person.
func isSystemProfile(name string) bool {
	lower := strings.ToLower(name)
	if systemProfiles[lower] {
		return true
	}
	for _, p := range systemProfilePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// LocalUsers lists the account profiles present on this machine.
//
// Profile directories are used rather than the account database because they
// answer the question that actually matters here: where can credentials be
// delivered. An account that has never signed in has no profile and no place
// to receive anything.
func (m *localMachine) LocalUsers() []string {
	entries, err := os.ReadDir(usersRoot())
	if err != nil {
		return nil
	}
	var users []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if isSystemProfile(name) {
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		users = append(users, name)
	}
	sort.Strings(users)
	return users
}

func (m *localMachine) ProfileDir(user string) string {
	return filepath.Join(usersRoot(), user)
}

// usersRoot is where Windows keeps profile directories.
func usersRoot() string {
	if v := os.Getenv("AIENVMGR_USERS_ROOT"); v != "" {
		return v // test hook
	}
	if v := os.Getenv("SystemDrive"); v != "" {
		return filepath.Join(v+`\`, "Users")
	}
	return filepath.FromSlash("/home")
}

// localApplier performs the real side effects on this machine.
type localApplier struct{}

func (localApplier) ApplyPolicy(p model.Policy) error { return policy.Apply(p) }

func (localApplier) ApplyAppLocker(paths []string, mode string) error {
	return policy.ApplyAppLocker(paths, mode)
}

func (localApplier) DeployCreds(profileDir string, set model.CredentialSet) (int, map[string]string, []string, error) {
	rep, err := creds.WriteToProfileReport(profileDir, set)
	if err != nil {
		return rep.Written, nil, nil, err
	}
	if rep.Written == 0 {
		return 0, nil, nil, nil
	}

	// An entry this agent does not recognize means the console is delivering
	// something newer than this build knows how to place. Reporting it is the
	// difference between "the employee is missing a file" being visible in
	// admin status and it looking like a perfectly clean delivery.
	if len(rep.Skipped) > 0 {
		return rep.Written, nil, nil, fmt.Errorf("this agent does not know how to place %s; update the agent",
			strings.Join(rep.Skipped, ", "))
	}

	// The agent runs as SYSTEM, so freshly written files would otherwise not
	// be readable from the employee's own session. A failure here is reported
	// rather than swallowed: the files exist but the employee cannot read
	// them, which looks exactly like "credentials were never delivered" from
	// their side and would otherwise be invisible in admin status.
	user := filepath.Base(strings.TrimRight(filepath.Clean(profileDir), `\/`))
	var failed []string
	for _, sub := range []string{".codex", ".claude", ".claude.json"} {
		target := filepath.Join(profileDir, sub)
		if _, statErr := os.Stat(target); statErr != nil {
			continue
		}
		if err := creds.GrantAccess(target, user); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", sub, err))
		}
	}
	if len(failed) > 0 {
		return rep.Written, nil, nil, fmt.Errorf("credentials written but not readable by %s (%s)",
			user, strings.Join(failed, "; "))
	}

	// Restart the tools only when a login actually changed on disk.
	//
	// The kill is a taskkill /F: it takes an employee's in-progress work with
	// no chance to save. The archive carries every entry ever published for
	// them, so a login from months ago rides along with today's catalog and
	// "the archive contains a login" is true of every delivery. The decision
	// therefore rests on which bytes moved (creds.NeedsToolRestart), not on
	// what the archive happened to contain.
	if creds.NeedsToolRestart(rep) {
		creds.StopAITools()
	}
	return rep.Written, rep.Placed, rep.Merged, nil
}

// RemoveCreds deletes an offboarded employee's logins from their profile.
func (localApplier) RemoveCreds(profileDir string) (int, error) {
	n, err := creds.Remove(profileDir)
	if n > 0 {
		// The tools keep the tokens in memory, so a running session would
		// carry on working until it restarts.
		creds.StopAITools()
	}
	return n, err
}

// localState reports what this machine currently has applied, without
// contacting the store and without changing anything.
//
// This backs `agent.exe status`. Running a full sync to answer "what is the
// state here?" would rewrite the registry and restart the employee's AI
// tools -- a diagnostic command must not do that.
func localState(m *localMachine, version string) localReport {
	var errs []string

	pol, err := policy.Current()
	if err != nil {
		errs = append(errs, fmt.Sprintf("read applied policy: %v", err))
	}

	// AppLockerAllowPaths is read separately from Current(): it is not "the
	// browser block list applied", it is "what is really on this machine's
	// AppLocker policy right now", and it needs to say so when the read
	// itself fails rather than quietly reporting zero. appLockerAllowUnknown
	// distinguishes "confirmed empty" (0) from "could not read" -- the very
	// distinction the operator who diagnosed this incident needed and did
	// not have, from pasting exactly this line of `agent status` output.
	appLockerAllow := appLockerAllowUnknown
	if paths, err := policy.LocalAppLockerPaths(); err != nil {
		errs = append(errs, fmt.Sprintf("read local AppLocker policy: %v", err))
	} else {
		appLockerAllow = len(paths)
	}

	// Which local profiles actually hold AI logins. Local inspection cannot
	// know which employee this machine is assigned to -- that lives in the
	// store -- but it can say who on this box has credentials in place.
	users := m.LocalUsers()
	var withCreds []string
	for _, u := range users {
		if len(creds.Present(m.ProfileDir(u))) > 0 {
			withCreds = append(withCreds, u)
		}
	}

	return localReport{
		Machine:             m.Name(),
		LocalUsers:          users,
		UsersWithCreds:      withCreds,
		AgentVersion:        version,
		BlockEnabled:        pol.BlockEnabled,
		BlockedDomains:      pol.BlockedDomains,
		AppLockerMode:       status.AppLockerMode(),
		AppLockerAllowPaths: appLockerAllow,
		Errors:              errs,
	}
}

// appLockerAllowUnknown marks localReport.AppLockerAllowPaths as "the local
// AppLocker policy could not be read" rather than a confirmed empty list.
// See formatAppLockerAllow in main.go, which turns this into "?" for
// display.
const appLockerAllowUnknown = -1

// localReport is what can be learned from the machine alone. It is
// deliberately not model.Status: that type describes a completed sync and
// carries fields (bound user, etags, last sync) that local inspection has no
// way to know, and reporting them as empty would read as "not bound" and
// "never synced" rather than "not known from here".
type localReport struct {
	Machine        string
	LocalUsers     []string
	UsersWithCreds []string
	AgentVersion   string
	BlockEnabled   bool
	BlockedDomains []string
	AppLockerMode  string
	// AppLockerAllowPaths is the count of managed AppLocker rules actually
	// read off this machine, or appLockerAllowUnknown if the read failed
	// (the reason lands in Errors). Format with formatAppLockerAllow.
	AppLockerAllowPaths int
	Errors              []string
}
