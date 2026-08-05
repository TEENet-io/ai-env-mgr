// Package agentcore holds the agent's sync logic.
//
// It is deliberately free of OSS and Windows specifics: the store, the local
// side effects and the machine's own facts all arrive as interfaces or
// functions, so the whole cycle can be tested with fakes on any platform.
package agentcore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TEENet-io/airlock/internal/creds"
	"github.com/TEENet-io/airlock/internal/model"
	"github.com/TEENet-io/airlock/internal/ossclient"
	"github.com/TEENet-io/airlock/internal/status"
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
}

// Applier performs the local side effects: writing browser policy into the
// registry, dropping credentials into the employee's profile, and removing
// them again when the employee is offboarded.
type Applier interface {
	ApplyPolicy(p model.Policy) error
	DeployCreds(profileDir string, set model.CredentialSet) (int, error)
	RemoveCreds(profileDir string) (int, error)
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

// Marker file names inside the state directory.
const (
	policyMarkerFile   = "policy.etag"
	credsMarkerFile    = "credentials.etag"
	lastSyncMarkerFile = "last-sync"
)

// Syncer runs one sync cycle.
type Syncer struct {
	Store   Store
	Applier Applier
	Machine Machine

	Version  string
	StateDir string // where etag and timestamp markers are cached

	// FallbackInterval is used when the policy does not specify one.
	FallbackInterval int
}

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

// loadBinding reads which employee this machine serves.
// A missing binding is not an error: a freshly created machine simply has not
// been assigned yet, and reports itself so the administrator can bind it.
func (s *Syncer) loadBinding(machine string) (model.Binding, bool) {
	data, _, err := s.Store.Get(ossclient.BindingKey(machine))
	if err != nil {
		return model.Binding{}, false
	}
	var b model.Binding
	if err := json.Unmarshal(data, &b); err != nil || b.User == "" {
		return model.Binding{}, false
	}
	return b, true
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
	pol := model.Policy{}
	policyETag := ""
	credsETag := ""
	credsApplied := false

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
		if err := json.Unmarshal(data, &pol); err != nil {
			errs = append(errs, fmt.Sprintf("policy parse: %v", err))
		} else if etag != "" && etag == s.readMarker(policyMarkerFile) {
			// Unchanged since the last cycle; skip the registry writes.
		} else if err := s.Applier.ApplyPolicy(pol); err != nil {
			errs = append(errs, fmt.Sprintf("policy apply: %v", err))
		} else {
			s.writeMarker(policyMarkerFile, etag)
		}
	}

	binding, bound := s.loadBinding(machine)
	boundUserExists := false

	if !bound {
		// The policy above is already in force. Only credentials need an
		// employee to deliver to, so the machine reports itself and waits.
		errs = append(errs, "no binding: this machine has not been assigned to a user yet")
	} else {
		boundUserExists = containsFold(localUsers, binding.User)
		if !boundUserExists {
			errs = append(errs, fmt.Sprintf("bound user %q has no profile on this machine", binding.User))
		}

		// ---- credentials ----
		// Skipped when the profile is absent: there is nowhere to put them.
		credsKey := ossclient.UserKey(binding.User, "credentials.zip")
		if !boundUserExists {
			errs = append(errs, "credentials skipped: no profile to deliver them to")
		} else if etag, exists, err := s.Store.Head(credsKey); err != nil {
			errs = append(errs, fmt.Sprintf("credentials: %v", err))
		} else if !exists {
			// Offboarding reaches the machine through the object going away.
			switch n, rmErr := s.Applier.RemoveCreds(s.Machine.ProfileDir(binding.User)); {
			case rmErr != nil:
				errs = append(errs, fmt.Sprintf("credentials revoke: %v", rmErr))
			case n > 0:
				errs = append(errs, fmt.Sprintf("credentials revoked: removed %d file(s) for %q", n, binding.User))
				s.writeMarker(credsMarkerFile, "")
			default:
				// Nothing published and nothing to remove: the employee
				// exists but the administrator has not signed in for them
				// yet. Worth saying so -- a bound machine with no logins
				// looks fine from every other angle.
				errs = append(errs, fmt.Sprintf("no credentials published for %q yet", binding.User))
			}
			credsETag = ""
		} else if etag != "" && etag == s.readMarker(credsMarkerFile) {
			// Already delivered in an earlier cycle. Do not download it:
			// there is nothing to learn and it is the machine's most
			// sensitive object.
			credsETag = etag
			credsApplied = true
		} else if data, _, err := s.Store.Get(credsKey); err != nil {
			errs = append(errs, fmt.Sprintf("credentials: %v", err))
		} else {
			credsETag = etag
			if set, err := creds.Unpack(data); err != nil {
				errs = append(errs, fmt.Sprintf("credentials unpack: %v", err))
			} else if n, err := s.Applier.DeployCreds(s.Machine.ProfileDir(binding.User), set); err != nil {
				errs = append(errs, fmt.Sprintf("credentials deploy: %v", err))
			} else if n > 0 {
				credsApplied = true
				s.writeMarker(credsMarkerFile, etag)
			} else {
				// The archive held nothing we recognise. Saying so beats
				// retrying forever with no trace in admin status.
				errs = append(errs, "credentials: archive contained no recognised files")
			}
		}
	}

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
		Errors:          errs,
	})

	if out, err := status.Marshal(st); err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("status encode: %v", err))
	} else if err := s.Store.Put(ossclient.StatusKey(machine), out); err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("status upload: %v", err))
	}

	// Record the wall-clock time so a later wake-up can tell how stale we are.
	s.writeMarker(lastSyncMarkerFile, time.Now().UTC().Format(time.RFC3339))

	return st, nil
}

// NextInterval reports how long to wait before the next cycle, based on the
// interval the last status reported.
func (s *Syncer) NextInterval(st model.Status) time.Duration {
	minutes := model.ClampInterval(st.SyncIntervalMinutes, s.FallbackInterval)
	return time.Duration(minutes) * time.Minute
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
