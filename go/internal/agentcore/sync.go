// Package agentcore holds the agent's sync logic.
//
// It is deliberately free of OSS and Windows specifics: the store, the local
// side effects and the machine's own facts all arrive as interfaces or
// functions, so the whole cycle can be tested with fakes on any platform.
package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/policy"
	"github.com/TEENet-io/ai-env-mgr/internal/status"
)

// Store is the subset of the object store the agent needs.
//
// Head exists so an unchanged object need not be downloaded at all. That
// matters most for credentials.zip: it holds the employee's live AI tokens,
// and at a short sync interval fetching it every cycle would move those
// secrets across the network hundreds of times a day to discover, each time,
// that nothing had changed.
type Store interface {
	Head(key string) (etag string, exists bool, err error)
	Get(key string) ([]byte, string, error)
	Put(key string, data []byte) error
	// GetToFile streams an object to disk and returns its SHA-256. The Codex
	// installer is ~700 MB, and buffering that on a cloud desktop to deliver
	// an update to it would risk taking the machine down.
	GetToFile(key, dest string) (sha256hex string, err error)
}

// Applier performs the local side effects: writing browser policy into the
// registry, dropping credentials into the employee's profile, and removing
// them again when the employee is offboarded.
type Applier interface {
	ApplyPolicy(p model.Policy) error
	// DeployCreds writes the archive into the employee's profile and reports
	// what that did -- see Delivery.
	DeployCreds(profileDir string, set model.CredentialSet) (Delivery, error)
	RemoveCreds(profileDir string) (int, error)
	// StopCodex ends the AI tools running in one employee's session and
	// reports how many processes it stopped. Nothing running is (0, nil), not
	// an error: most restarts land on somebody who did not have Codex open.
	//
	// It takes a user rather than acting machine-wide because these are
	// multi-session cloud desktops -- see creds.stopCodexArgs.
	StopCodex(user string) (killed int, err error)
	// ApplyAppLocker brings the machine's local AppLocker policy in line with
	// paths. It is a separate method rather than folded into ApplyPolicy
	// because it must NOT be gated by the policy ETag: ApplyPolicy is skipped
	// whenever the policy object has not changed, but the allow list is what
	// lets an employee launch tools installed outside %WINDIR%/%PROGRAMFILES%,
	// and the machine's own AppLocker XML can drift out from under the policy
	// object at any time (a re-run image script, a GPO refresh, a hand edit).
	// It is called on every cycle so the machine can self-heal on its own.
	//
	// mode is the enforcement mode from the same policy object ("enforce",
	// "audit", or empty for unmanaged), applied in the same pass so the
	// fleet-wide off switch costs no extra work on the machine.
	ApplyAppLocker(paths []string, mode string) error
}

// Delivery is what one credential deploy actually did on the machine.
//
// It is a struct rather than a handful of return values because the facts it
// carries answer different questions -- what landed, what can be verified
// later, and what actually moved -- and a caller that confuses the last two
// either takes an employee's work away for nothing or leaves them running on
// a token that has been withdrawn.
type Delivery struct {
	// Written counts the files placed on disk, whether or not their contents
	// moved. Zero means the archive held nothing this build recognises.
	Written int

	// Placed maps a written file's path to the SHA-256 of the bytes that
	// reached disk, and Merged lists the files folded into what the employee
	// already had (checked for presence only). Together they are what lets a
	// later cycle tell a delivered file from one that was skipped or has
	// since been removed, which is what makes skipping an unchanged archive
	// safe.
	Placed map[string]string
	Merged []string

	// Changed names the entries whose bytes on disk are not what they were
	// before this delivery: files created, files rewritten, and files
	// replaced after somebody deleted them.
	//
	// This is what decides whether the employee's session is ended. A
	// redelivery is not evidence of anything on its own -- the archive is
	// re-fetched whenever its ETag moves or the local marker cannot be read,
	// and either can happen with every byte on disk already correct.
	Changed []string
}

// Machine describes what the agent can learn about the box it runs on.
// Both are injected so the sync logic stays testable off Windows.
type Machine interface {
	// Name is the hostname, which identifies this machine in the store.
	Name() string
	// LocalUsers lists the account profiles present on this machine.
	LocalUsers() []string
	// ProfileDir returns where a given account's profile lives.
	ProfileDir(user string) string
}

// administratorAccount is the built-in local admin. It is a profile like any
// other but never an employee, so machine-wide collection skips it.
const administratorAccount = "administrator"

// Marker file names inside the state directory.
const (
	policyMarkerFile   = "policy.etag"
	credsMarkerFile    = "credentials.etag"
	lastSyncMarkerFile = "last-sync"
	updateMarkerFile   = "update-target" // the last self-update version attempted
	// The ETags the last full sync saw, for ChangedSinceLastSync. Separate
	// from policyMarkerFile, which moves only when a policy was applied.
	policySeenMarkerFile  = "policy-seen.etag"
	bindingSeenMarkerFile = "binding-seen.etag"
	absentMarker          = "absent"
	// codexRestartMarkerFile lives in codexrestart.go, beside what writes it.
)

// Agent self-update states, reported in status.
const (
	AgentUpdatePending = "pending" // fetched and verified; applied after this report
	AgentUpdateFailed  = "failed"  // attempted in an earlier cycle and this is still the old binary
)

// effectiveTargets decides what this machine should be running. A target on
// the binding replaces the fleet target for that product, even when it says
// "nothing"; otherwise the fleet policy applies.
func effectiveTargets(pol model.Policy, b model.Binding) (agent, codex model.ReleaseTarget) {
	agent = model.ReleaseTarget{Version: pol.AgentUpdateVersion, SHA256: pol.AgentUpdateSHA256, Key: ossclient.AgentBinaryKey()}
	if b.AgentTarget != nil {
		agent = *b.AgentTarget
		if agent.Key == "" {
			agent.Key = ossclient.AgentBinaryKey()
		}
	}
	codex = model.ReleaseTarget{Version: pol.CodexVersion, SHA256: pol.CodexSHA256, Key: pol.CodexKey}
	if b.CodexTarget != nil {
		codex = *b.CodexTarget
	}
	return agent, codex
}

// targetMarker is what the one-attempt markers record: the version and the
// generation, so a new generation of the same version is a new attempt.
func targetMarker(t model.ReleaseTarget) string {
	return t.Version + "@" + strconv.Itoa(t.Generation)
}

// markerMatches reports whether a stored marker refers to this target.
// Agents before 1.2.16 wrote the bare version; for a fleet target
// (generation 0) that still means "tried", so an upgraded machine does not
// take one more run at a package it already refused.
func markerMatches(marker string, t model.ReleaseTarget) bool {
	return marker == targetMarker(t) || (t.Generation == 0 && marker == t.Version)
}

// Updater replaces the running agent binary with a newer one and restarts the
// service. It is a platform-specific side effect (Windows renames the exe and
// restarts the service), injected so the sync logic stays testable.
type Updater interface {
	ApplyUpdate(newBinary []byte) error
}

// Syncer runs one sync cycle.
type Syncer struct {
	Store   Store
	Applier Applier
	Machine Machine

	Version  string
	StateDir string // where etag and timestamp markers are cached

	// FallbackInterval is used when the policy does not specify one.
	FallbackInterval int

	// Collector uploads raw session files when policy.CollectEnabled is set.
	// Optional: nil means collection is not wired in (tests, older builds).
	Collector CollectRunner

	// Updater applies an agent self-update when the policy targets a version
	// other than this binary's. Optional: nil disables self-update.
	Updater Updater

	// Codex installs the repackaged Codex desktop when the policy targets a
	// version this machine does not have. Optional: nil disables it, which is
	// what every non-Windows build and every test gets.
	Codex CodexInstaller

	// mu guards lastStatus. ReportEvent runs on the service control handler's
	// goroutine, which can overlap the worker goroutine's RunOnce/Heartbeat, so
	// the shared last status must be locked.
	mu sync.Mutex
	// lastStatus is the most recent full status, reused by Heartbeat to refresh
	// the machine's "last seen" time between full syncs, and by ReportEvent to
	// stamp a lifecycle transition onto the same report.
	lastStatus model.Status
}

// eventReportTimeout bounds how long ReportEvent waits for the store. On
// suspend the OS is waiting for the service handler to return, so the report
// must not block sleep indefinitely if the network is already tearing down.
const eventReportTimeout = 3 * time.Second

func (s *Syncer) readMarker(name string) string {
	data, err := os.ReadFile(filepath.Join(s.StateDir, name))
	if err != nil {
		return ""
	}
	return string(data)
}

func (s *Syncer) writeMarker(name, value string) {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.StateDir, name), []byte(value), 0o600)
}

// LastSyncTime reports when the previous cycle finished, persisted across
// service restarts. A zero time means "never synced on this machine".
func (s *Syncer) LastSyncTime() time.Time {
	raw := s.readMarker(lastSyncMarkerFile)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// DueForSync reports whether the wall clock says a sync is overdue.
//
// The service loop also uses a ticker, but a ticker runs on the monotonic
// clock and stops advancing while the machine sleeps. Cloud desktops suspend
// often, so after a wake-up the ticker alone would leave the machine on a
// stale policy for the remainder of its interval. Comparing wall-clock times
// catches that, and also covers a missed power-event notification.
func (s *Syncer) DueForSync(interval time.Duration) bool {
	last := s.LastSyncTime()
	if last.IsZero() {
		return true
	}
	elapsed := time.Since(last)
	if elapsed < 0 {
		// The clock moved backwards; treat it as overdue rather than trusting it.
		return true
	}
	return elapsed >= interval
}

// loadBinding reads which employee this machine serves, and the machine's
// release targets, which ride on the same object.
//
// A missing binding is not an error: a freshly created machine simply has not
// been assigned yet, and reports itself so the administrator can bind it. An
// object with no user is "not bound" too, but its targets still count: a
// machine nobody is assigned to can still be told what to run.
func (s *Syncer) loadBinding(machine string) (model.Binding, bool) {
	data, etag, err := s.Store.Get(ossclient.BindingKey(machine))
	if err != nil {
		if errors.Is(err, ossclient.ErrNotFound) {
			s.writeMarker(bindingSeenMarkerFile, absentMarker)
		}
		return model.Binding{}, false
	}
	s.writeMarker(bindingSeenMarkerFile, etag)
	var b model.Binding
	if err := json.Unmarshal(data, &b); err != nil {
		return model.Binding{}, false
	}
	return b, b.User != ""
}

// ChangedSinceLastSync reports whether the policy or this machine's binding
// object has changed since the last full sync, and which. It costs two HEAD
// requests, which is what lets the sync interval be long: the console's
// changes -- a target, a binding, a revocation, a "sync now" -- reach the
// machine on the next heartbeat instead of the next interval. A store that
// cannot be reached answers "no": the scheduled sync is the fallback.
func (s *Syncer) ChangedSinceLastSync() (bool, string) {
	if etag, exists, err := s.Store.Head(ossclient.PolicyKey()); err == nil && exists {
		if seen := s.readMarker(policySeenMarkerFile); seen != "" && seen != etag {
			return true, "policy"
		}
	}
	etag, exists, err := s.Store.Head(ossclient.BindingKey(s.Machine.Name()))
	if err != nil {
		return false, ""
	}
	if !exists {
		etag = absentMarker
	}
	if seen := s.readMarker(bindingSeenMarkerFile); seen != "" && seen != etag {
		return true, "binding"
	}
	return false, ""
}

// RunOnce performs one full cycle: apply the machine-wide block policy, work
// out who this machine serves, deliver that employee's credentials, then
// report back.
//
// A failure in one step is recorded and the cycle continues: a broken policy
// object must not stop credentials from being delivered, and a failed status
// upload must not undo work that already landed on the machine.
func (s *Syncer) RunOnce() (model.Status, error) {
	machine := s.Machine.Name()
	localUsers := s.Machine.LocalUsers()

	var errs []string
	// Warnings are states rather than failures: reporting them as errors made
	// an ordinary onboarding step look like something had gone wrong.
	var warns []string
	pol := model.Policy{}
	policyETag := ""
	credsETag := ""
	credsApplied := false
	var codexRestart codexRestartMark
	// One interruption per cycle: a delivery and a pending request both end
	// the same session, and doing it twice in one pass would take whatever
	// the employee had reopened in between.
	var sweep codexSweep

	// ---- policy ----
	// Applied first and unconditionally. The block is written into HKLM and
	// protects the whole machine no matter who logs in, so it must not depend
	// on knowing which employee this machine serves: a box that has just been
	// created from the image, or whose employee has no profile yet, is exactly
	// the one that must not be left open.
	if data, etag, err := s.Store.Get(ossclient.PolicyKey()); err != nil {
		errs = append(errs, fmt.Sprintf("policy: %v", err))
	} else {
		policyETag = etag
		s.writeMarker(policySeenMarkerFile, etag)
		if err := json.Unmarshal(data, &pol); err != nil {
			errs = append(errs, fmt.Sprintf("policy parse: %v", err))
		} else {
			if etag != "" && etag == s.readMarker(policyMarkerFile) {
				// Unchanged since the last cycle; skip the registry writes.
			} else if err := s.Applier.ApplyPolicy(pol); err != nil {
				errs = append(errs, fmt.Sprintf("policy apply: %v", err))
			} else {
				s.writeMarker(policyMarkerFile, etag)
			}
			// Not gated by the ETag: see the Applier.ApplyAppLocker comment.
			if err := s.Applier.ApplyAppLocker(pol.AppLockerAllowPaths, pol.AppLockerMode); err != nil {
				if errors.Is(err, policy.ErrAppLockerNotDeployed) {
					warns = append(warns, fmt.Sprintf("applocker: %v", err))
				} else {
					errs = append(errs, fmt.Sprintf("applocker: %v", err))
				}
			}
		}
	}

	binding, bound := s.loadBinding(machine)
	boundUserExists := false

	if !bound {
		// The policy above is already in force. Only credentials need an
		// employee to deliver to, so the machine reports itself and waits.
		warns = append(warns, "no binding: this machine has not been assigned to a user yet")
	} else {
		boundUserExists = containsFold(localUsers, binding.User)
		if !boundUserExists {
			warns = append(warns, fmt.Sprintf("bound user %q has no profile on this machine", binding.User))
		}

		// ---- credentials ----
		// Skipped when the profile is absent: there is nowhere to put them.
		credsKey := ossclient.UserKey(binding.User, "credentials.zip")
		if !boundUserExists {
			warns = append(warns, "credentials skipped: no profile to deliver them to")
		} else if etag, exists, err := s.Store.Head(credsKey); err != nil {
			errs = append(errs, fmt.Sprintf("credentials: %v", err))
		} else if !exists {
			// Offboarding reaches the machine through the object going away.
			switch n, rmErr := s.Applier.RemoveCreds(s.Machine.ProfileDir(binding.User)); {
			case rmErr != nil:
				errs = append(errs, fmt.Sprintf("credentials revoke: %v", rmErr))
			case n > 0:
				warns = append(warns, fmt.Sprintf("credentials revoked: removed %d file(s) for %q", n, binding.User))
				s.writeMarker(credsMarkerFile, "")
				// The tools hold the token in memory, so a session left
				// running would keep working against an account that has
				// just been closed. Deleting the files is only half of a
				// revocation until the process that cached them is gone.
				warns = append(warns, s.stopCodex(binding.User, "a credential revocation", &sweep))
			default:
				// Nothing published and nothing to remove: the employee
				// exists but the administrator has not signed in for them
				// yet. Worth saying so -- a bound machine with no logins
				// looks fine from every other angle.
				warns = append(warns, fmt.Sprintf("no credentials published for %q yet", binding.User))
			}
			credsETag = ""
		} else if mark, ok := s.readCredsMark(); ok && etag != "" && etag == mark.ETag && s.credsIntact(mark) {
			// Already delivered in an earlier cycle AND every file it placed
			// is still on disk unchanged. Do not download it: there is
			// nothing to learn and it is the machine's most sensitive object.
			//
			// The verification is what makes the ETag safe to trust. On its
			// own it records only which archive was fetched, not what came of
			// it -- so an entry an older build had no target for, or a file
			// the employee later deleted, would leave the machine short of a
			// file forever with the marker still claiming success.
			credsETag = etag
			credsApplied = true
		} else if data, _, err := s.Store.Get(credsKey); err != nil {
			errs = append(errs, fmt.Sprintf("credentials: %v", err))
		} else {
			credsETag = etag
			if set, err := creds.Unpack(data); err != nil {
				errs = append(errs, fmt.Sprintf("credentials unpack: %v", err))
			} else if d, err := s.Applier.DeployCreds(s.Machine.ProfileDir(binding.User), set); err != nil {
				errs = append(errs, fmt.Sprintf("credentials deploy: %v", err))
			} else if d.Written > 0 {
				credsApplied = true
				s.writeCredsMark(etag, d.Placed, d.Merged)
				// Name the files. Without this a delivery leaves only an
				// ETag behind, so "the archive was fetched" and "the file
				// the employee needs is on disk" cannot be told apart -- the
				// exact gap that let a silently skipped entry go unnoticed.
				log.Printf("credentials: placed %d file(s): %s", d.Written,
					strings.Join(append(baseNames(d.Placed), baseNames(d.Merged)...), ", "))
				// A catalog-only update is safe to pick up on the next launch.
				// Gateway permissions already enforce removed models. Other
				// changes (especially tokens) still need the old process ended.
				restart := false
				for _, name := range d.Changed {
					if name != model.PathCodexModels {
						restart = true
					}
				}
				if restart {
					warns = append(warns, s.stopCodex(binding.User, "a credential update", &sweep))
				} else if len(d.Changed) > 0 {
					warns = append(warns, "model catalog updated; restart Codex when convenient to load it; running task was not stopped")
				}
			} else {
				// The archive held nothing we recognise. Saying so beats
				// retrying forever with no trace in admin status.
				errs = append(errs, "credentials: archive contained no recognised files")
			}
		}

		// ---- one-shot restart request ----
		// After the credentials, so a cycle that delivers a new package and
		// carries a pending request does the delivery first and the employee
		// gets one interruption covering both: the sweep above, if it ran,
		// is what the request records as its outcome.
		codexRestart = s.runCodexRestart(binding, boundUserExists, sweep, &warns)
	}

	// ---- collection ----
	// Machine-wide and independent of the binding: every real user profile on
	// the box is collected into its own data_collect/{user}/, so a shared
	// desktop with several accounts has all of them captured, not just the one
	// it is bound to. LocalUsers already drops system profiles; the built-in
	// Administrator is skipped here because it is not an employee.
	collectUploaded := 0
	if pol.CollectEnabled && s.Collector != nil {
		for _, u := range localUsers {
			if strings.EqualFold(u, administratorAccount) {
				continue
			}
			cr := s.Collector.CollectOnce(u, pol.CollectQuietSeconds, pol.CollectSince)
			collectUploaded += cr.Uploaded
			errs = append(errs, cr.Errors...)
		}
	}

	// ---- Codex desktop ----
	// Before the agent's own update: a self-update restarts this process, and
	// a Codex install interrupted halfway is worse than one that waits a cycle.
	agentTarget, codexTarget := effectiveTargets(pol, binding)
	codex := s.updateCodex(codexTarget, &errs)

	// ---- self-update: download + verify ----
	// The binary is fetched and checksummed now so any problem surfaces in this
	// cycle's status, but it is applied only after status is uploaded (below),
	// so the machine's pre-update state is recorded before the restart.
	pendingUpdate, updateState := s.prepareUpdate(agentTarget, &errs)

	// ---- status ----
	interval := model.ClampInterval(pol.SyncIntervalMinutes, s.FallbackInterval)
	st := status.Build(status.Report{
		Machine:         machine,
		BoundUser:       binding.User,
		LocalUsers:      localUsers,
		BoundUserExists: boundUserExists,
		Version:         s.Version,
		Policy:          pol,
		PolicyETag:      policyETag,
		CredsETag:       credsETag,
		Interval:        interval,
		CredsApplied:    credsApplied,
		CollectEnabled:  pol.CollectEnabled,
		CollectUploaded: collectUploaded,
		CodexVersion:    codex.Version,
		CodexState:      codex.State,
		CodexRestart: status.CodexRestart{
			Nonce: codexRestart.Nonce, At: codexRestart.At, Note: codexRestart.Note,
		},
		CodexTarget:           codexTarget.Version,
		CodexTargetGeneration: codexTarget.Generation,
		CodexDeferReason:      codex.Reason,
		AgentUpdateTarget:     agentTarget.Version,
		AgentUpdateGeneration: agentTarget.Generation,
		AgentUpdateState:      updateState,
		Errors:                errs,
		Warnings:              warns,
	})

	if out, err := status.Marshal(st); err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("status encode: %v", err))
	} else if err := s.Store.Put(ossclient.StatusKey(machine), out); err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("status upload: %v", err))
	}

	// Remember this status so Heartbeat can refresh "last seen" cheaply between
	// full syncs, without re-pulling policy or touching the employee's tools.
	// A completed sync means the agent is running normally, so any earlier
	// lifecycle event (a suspend that never actually slept, say) is now stale:
	// Build already leaves LastEvent empty, which clears it.
	s.mu.Lock()
	s.lastStatus = st
	s.mu.Unlock()

	// Record the wall-clock time so a later wake-up can tell how stale we are.
	s.writeMarker(lastSyncMarkerFile, time.Now().UTC().Format(time.RFC3339))

	// ---- self-update: apply last ----
	// Applied after status is uploaded so the pre-update report is sent first.
	// The target is marked before applying so a binary that fails to take does
	// not loop: the admin must publish a new version (or clear it) to retry.
	if pendingUpdate != nil {
		s.writeMarker(updateMarkerFile, targetMarker(agentTarget))
		if err := s.Updater.ApplyUpdate(pendingUpdate); err != nil {
			return st, fmt.Errorf("apply update to %s: %w", agentTarget.Version, err)
		}
		// On success the process is being replaced and restarted; the next
		// cycle runs from the new binary and reports the new version.
	}

	return st, nil
}

// prepareUpdate downloads and verifies the targeted agent binary, returning
// the bytes to apply (or nil) and the state to report. It applies nothing
// itself. A checksum mismatch or a fetch failure is appended to errs so it
// shows up in this cycle's status, and a target already attempted is skipped
// so a bad binary cannot crash-loop the machine.
func (s *Syncer) prepareUpdate(target model.ReleaseTarget, errs *[]string) ([]byte, string) {
	if target.Version == "" || target.Version == s.Version || s.Updater == nil {
		return nil, ""
	}
	if markerMatches(s.readMarker(updateMarkerFile), target) {
		// Tried already and this is still the old binary: the update did not
		// take. Say so every cycle until the console moves the generation.
		return nil, AgentUpdateFailed
	}
	data, _, err := s.Store.Get(target.Key)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("update: fetch binary: %v", err))
		return nil, AgentUpdateFailed
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, target.SHA256) {
		*errs = append(*errs, fmt.Sprintf("update: checksum mismatch (got %s, want %s); not applying", got, target.SHA256))
		s.writeMarker(updateMarkerFile, targetMarker(target)) // one attempt per generation, like Codex
		return nil, AgentUpdateFailed
	}
	return data, AgentUpdatePending
}

// UploadLog stores the recent agent log for this machine so the admin can read
// it remotely (see `admin log <machine>`). Best effort: the caller logs and
// ignores failures rather than failing the sync over a log upload.
func (s *Syncer) UploadLog(tail []byte) error {
	return s.Store.Put(ossclient.LogKey(s.Machine.Name()), tail)
}

// Heartbeat re-uploads the machine's last status with a fresh timestamp, so
// the admin sees it as alive between full syncs.
//
// It is deliberately lightweight: it pulls no policy, delivers no credentials
// and never touches the employee's running tools. It only refreshes "last
// seen", which is what lets the sync interval stay long (cheap, infrequent
// config pulls) while liveness stays fresh. A no-op before the first full sync,
// since there is no status to refresh yet.
func (s *Syncer) Heartbeat() error {
	s.mu.Lock()
	if s.lastStatus.Machine == "" {
		s.mu.Unlock()
		return nil
	}
	st := s.lastStatus
	s.mu.Unlock()

	st.LastSync = time.Now().UTC().Format(time.RFC3339)
	// A heartbeat is proof the agent is alive and running, so it clears any
	// lifecycle event: a "suspend" the machine reported but then did not act on
	// must not linger and make a running machine look asleep.
	st.LastEvent = ""
	st.LastEventAt = ""
	out, err := status.Marshal(st)
	if err != nil {
		return fmt.Errorf("heartbeat encode: %w", err)
	}
	if err := s.Store.Put(ossclient.StatusKey(st.Machine), out); err != nil {
		return fmt.Errorf("heartbeat upload: %w", err)
	}
	s.mu.Lock()
	s.lastStatus = st
	s.mu.Unlock()
	return nil
}

// ReportEvent stamps a lifecycle transition (the machine is about to suspend,
// or the service is stopping) onto the machine's last status and re-uploads it,
// so the admin can tell an orderly departure apart from a crash. It is a no-op
// before the first full sync, since there is no status to stamp yet.
//
// It is best effort and bounded: on suspend the OS is waiting for the service
// handler to return, so a store that has already lost the network must not hold
// sleep open. A failed or timed-out upload simply means the admin falls back to
// the graduated-staleness view instead of the positive marker.
func (s *Syncer) ReportEvent(event string) {
	s.mu.Lock()
	if s.lastStatus.Machine == "" {
		s.mu.Unlock()
		return
	}
	st := s.lastStatus
	s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	st.LastSync = now
	st.LastEvent = event
	st.LastEventAt = now
	out, err := status.Marshal(st)
	if err != nil {
		return
	}

	// Bound the upload: run it off the calling goroutine and give up after
	// eventReportTimeout so a dying network cannot delay suspend or stop.
	//
	// The outcome is logged rather than discarded. Without it, a machine that
	// shows as merely offline is unexplainable after the fact: silence looks
	// identical whether the OS never delivered the event, or it did and the
	// upload could not finish before the machine went away. The log survives
	// the sleep and is uploaded on the next sync after waking, which is when
	// somebody is asking the question.
	done := make(chan error, 1)
	go func() { done <- s.Store.Put(ossclient.StatusKey(st.Machine), out) }()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("lifecycle event %q: report FAILED: %v", event, err)
		} else {
			log.Printf("lifecycle event %q: reported", event)
		}
	case <-time.After(eventReportTimeout):
		log.Printf("lifecycle event %q: report TIMED OUT after %s -- the admin will see this machine as offline",
			event, eventReportTimeout)
	}

	s.mu.Lock()
	s.lastStatus = st
	s.mu.Unlock()
}

// NextInterval reports how long to wait before the next cycle, based on the
// interval the last status reported.
func (s *Syncer) NextInterval(st model.Status) time.Duration {
	minutes := model.ClampInterval(st.SyncIntervalMinutes, s.FallbackInterval)
	return time.Duration(minutes) * time.Minute
}

// codexSweep records this cycle's kill so a second one is not needed. A zero
// value means nothing has been stopped yet in this pass.
type codexSweep struct {
	done   bool
	killed int
	err    error
}

// stopCodex ends the bound employee's AI tools, records the outcome in sweep
// and returns the line to put in the machine's warnings either way.
//
// It returns a warning rather than an error even when taskkill fails: the
// credentials are already on disk (or already gone), which is the part that
// had to succeed. A kill that did not happen means the employee keeps a stale
// session until they restart the tool themselves -- worth saying out loud,
// not worth reporting the whole cycle as failed over.
func (s *Syncer) stopCodex(user, reason string, sweep *codexSweep) string {
	killed, err := s.Applier.StopCodex(user)
	*sweep = codexSweep{done: true, killed: killed, err: err}
	if err != nil {
		return fmt.Sprintf("codex restart for %q after %s FAILED: %v", user, reason, err)
	}
	return fmt.Sprintf("codex restarted for %q after %s (%d killed)", user, reason, killed)
}

// containsFold reports membership ignoring case, as Windows account names do.
func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
