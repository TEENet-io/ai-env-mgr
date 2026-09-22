package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/catalog"
	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

// ObjectStore is the part of OSS the export needs.
type ObjectStore interface {
	Get(key string) ([]byte, string, error)
	Put(key string, data []byte) error
	Delete(key string) error
	// Copy duplicates an object server-side; the export uses it to put the
	// chosen agent build behind the fixed key the fleet reads.
	Copy(src, dst string) error
}

// ModelCatalog is the gateway's list of routable models, used to build the
// picker's models.json.
type ModelCatalog interface {
	Models(ctx context.Context) ([]litellm.Model, error)
}

// OSSExport writes the objects the agents read.
//
// This is the compatibility bridge, and it is meant to be temporary. The
// database is authoritative; OSS is a publishing format that the agents in the
// field still speak. Once every machine runs an agent that talks to the API
// (phase 2), this handler and the objects it writes go away, and OSS goes back
// to being where large files live.
//
// It is written to be re-runnable at any moment: the export always reproduces
// the whole object from the current state rather than applying a change. A
// retry, a duplicate task, or a run that happens out of order therefore all
// converge on the same bytes.
type OSSExport struct {
	Store   repo.Store
	Objects ObjectStore
	// Notifier, when set, is woken after the objects are written: this is
	// the authoritative wake, the one that follows every change, because
	// every change queues an export.
	Notifier Notifier
	Keyring  secrets.Keyring
	Catalog  ModelCatalog
	// GatewayBaseURL is what goes into every employee's config.toml. It is the
	// public gateway address: this string is what Codex on a desktop will
	// connect to, so a loopback address here silently breaks every machine.
	GatewayBaseURL string
}

// exportPayload is what the task carries. Exactly one of the three is set, and
// which one decides what is rewritten.
type exportPayload struct {
	PolicyVersion int64  `json:"policy_version,omitempty"`
	EmployeeID    string `json:"employee_id,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
}

// Run rewrites whatever the task points at.
func (h OSSExport) Run(ctx context.Context, task repo.Task) (Result, error) {
	var payload exportPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return Result{}, Permanent(fmt.Errorf("unreadable task payload: %w", err))
	}
	var subject string
	switch {
	case payload.PolicyVersion > 0:
		subject = "policy"
	case payload.DeviceID != "":
		subject = "device:" + payload.DeviceID
	case payload.EmployeeID != "":
		subject = "employee:" + payload.EmployeeID
	default:
		return Result{}, Permanent(errors.New("the task says nothing about what to export"))
	}

	// One export per subject at a time, across every worker. The export reads
	// the database and then writes an object the database cannot see; two of
	// them interleaved -- a second worker, or one that took over an expired
	// lease while the first was still writing -- can leave the older bytes in
	// the bucket. The lock is held for the whole read-then-write.
	var result Result
	err := h.Store.InTx(ctx, func(tx repo.Store) error {
		if err := tx.Lock(ctx, "oss_export:"+subject); err != nil {
			return err
		}
		locked := h
		locked.Store = tx
		var err error
		switch {
		case payload.PolicyVersion > 0:
			result, err = locked.exportPolicy(ctx, payload.PolicyVersion)
		case payload.DeviceID != "":
			result, err = locked.exportBinding(ctx, payload.DeviceID)
		default:
			result, err = locked.exportEmployee(ctx, payload.EmployeeID)
		}
		return err
	})
	if err == nil {
		h.notify(ctx, payload)
	}
	return result, err
}

// Notifier wakes agents waiting on the console for their configuration.
type Notifier interface {
	Wake(deviceIDs ...string)
	WakeAll()
}

// notify wakes whoever the export touched: everybody for the policy, one
// machine for a binding, an employee's machines for their credentials.
func (h OSSExport) notify(ctx context.Context, payload exportPayload) {
	if h.Notifier == nil {
		return
	}
	switch {
	case payload.PolicyVersion > 0:
		h.Notifier.WakeAll()
	case payload.DeviceID != "":
		h.Notifier.Wake(payload.DeviceID)
	case payload.EmployeeID != "":
		bindings, err := h.Store.Bindings().OpenByEmployee(ctx, payload.EmployeeID)
		if err != nil {
			return
		}
		ids := make([]string, 0, len(bindings))
		for _, b := range bindings {
			ids = append(ids, b.DeviceID)
		}
		h.Notifier.Wake(ids...)
	}
}

// exportPolicy writes the fleet policy.
//
// It writes whatever is current rather than the version the task names. Two
// publishes in quick succession queue two exports, and the older one must not
// be able to put the earlier policy back after the newer one landed.
func (h OSSExport) exportPolicy(ctx context.Context, requested int64) (Result, error) {
	current, err := h.Store.Policies().Current(ctx)
	if errors.Is(err, repo.ErrNotFound) {
		return Result{}, Permanent(errors.New("no policy has been published"))
	}
	if err != nil {
		return Result{}, err
	}
	var pol model.Policy
	if err := json.Unmarshal(current.Content, &pol); err != nil {
		return Result{}, Permanent(fmt.Errorf("policy version %d is not readable: %w", current.Version, err))
	}
	if pol.AgentUpdateVersion != "" {
		artifact, err := h.Store.Releases().ArtifactByVersion(ctx, repo.ProductAgent, pol.AgentUpdateVersion)
		switch {
		case err == nil:
			// Before the policy, every time: the fixed key must hold the
			// build the policy names before any agent reads the policy.
			if err := h.Objects.Copy(artifact.ObjectKey, ossclient.AgentBinaryKey()); err != nil {
				return Result{}, ClassError("oss_copy", err)
			}
		case errors.Is(err, repo.ErrNotFound):
			// A version set before the version library existed: the fixed
			// key was written directly and is left as it is.
		default:
			return Result{}, err
		}
	}
	if err := h.Objects.Put(ossclient.PolicyKey(), current.Content); err != nil {
		return Result{}, ClassError("oss_write", err)
	}
	note := fmt.Sprintf("policy version %d", current.Version)
	if current.Version != requested {
		note += fmt.Sprintf(" (the task asked for %d, which has been superseded)", requested)
	}
	return Result{Note: note}, nil
}

// exportBinding writes, or removes, one machine's binding object. What
// goes in it is deviceconfig's answer, the same one the device API gives.
func (h OSSExport) exportBinding(ctx context.Context, deviceID string) (Result, error) {
	cfg, err := deviceconfig.Build(ctx, h.Store, deviceID)
	if errors.Is(err, repo.ErrNotFound) {
		return Result{}, Permanent(fmt.Errorf("machine %s no longer exists", deviceID))
	}
	if err != nil {
		return Result{}, err
	}
	if cfg.Forgotten {
		// Forgotten: both its binding and its status report go, so it
		// disappears from the console. A machine that is still switched on
		// will write a new status on its next sync, which is the cue that the
		// wrong one was forgotten.
		for _, key := range []string{ossclient.BindingKey(cfg.Hostname), ossclient.StatusKey(cfg.Hostname)} {
			if err := h.Objects.Delete(key); err != nil {
				return Result{}, ClassError("oss_delete", err)
			}
		}
		return Result{Note: "forgotten"}, nil
	}
	if !cfg.HasBinding {
		// Nobody is assigned to it and nothing is aimed at it. The agent
		// reads the absence as "not assigned yet" and keeps applying the
		// machine-wide policy, which is exactly right for a machine that has
		// just been taken back.
		if err := h.Objects.Delete(ossclient.BindingKey(cfg.Hostname)); err != nil {
			return Result{}, ClassError("oss_delete", err)
		}
		return Result{Note: "unbound"}, nil
	}

	note := cfg.Hostname + " -> nobody"
	if cfg.Bound {
		note = cfg.Hostname + " -> " + cfg.Binding.User
	}
	if cfg.Binding.AgentTarget != nil {
		note += fmt.Sprintf(", agent %s gen %d", cfg.Binding.AgentTarget.Version, cfg.Binding.AgentTarget.Generation)
	}
	if cfg.Binding.CodexTarget != nil {
		note += fmt.Sprintf(", codex %s gen %d", cfg.Binding.CodexTarget.Version, cfg.Binding.CodexTarget.Generation)
	}
	data, err := cfg.BindingObject()
	if err != nil {
		return Result{}, Permanent(fmt.Errorf("encode binding: %w", err))
	}
	if err := h.Objects.Put(ossclient.BindingKey(cfg.Hostname), data); err != nil {
		return Result{}, ClassError("oss_write", err)
	}
	return Result{Note: note}, nil
}

// exportEmployee writes, or removes, one employee's credentials, and refreshes
// the binding of every machine assigned to them.
//
// Removing is how a revocation reaches a desktop: the agent sees the object is
// gone, deletes the local credentials and ends the Codex session that still
// holds the token in memory.
func (h OSSExport) exportEmployee(ctx context.Context, employeeID string) (Result, error) {
	employee, err := h.Store.Employees().ByID(ctx, employeeID)
	if errors.Is(err, repo.ErrNotFound) {
		return Result{}, Permanent(fmt.Errorf("employee %s no longer exists", employeeID))
	}
	if err != nil {
		return Result{}, err
	}

	codex, err := h.Store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway)
	hasCodex := err == nil
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return Result{}, err
	}
	set := model.CredentialSet{}

	// Nothing to deliver -- offboarded, or a fresh account whose token has not
	// arrived -- means no object. The agent reports the absence as "no
	// credentials published yet" for the second case and revokes for the first.
	if !employee.Active() || !hasCodex {
		if err := h.withdraw(ctx, employee); err != nil {
			return Result{}, err
		}
		if err := h.refreshBindings(ctx, employee); err != nil {
			return Result{}, err
		}
		return Result{Note: "credentials withdrawn"}, nil
	}

	allowed := 0
	if hasCodex {
		token, err := h.Keyring.Open(ctx, codex.Ciphertext, codex.KeyVersion,
			secrets.AAD("credential_versions", employee.ID, repo.PurposeCodexGateway))
		if err != nil {
			// A wrong master key or a tampered row. Retrying cannot help, and
			// publishing nothing is better than publishing something wrong.
			return Result{}, Permanent(fmt.Errorf("open the stored token for %s: %w", employee.WindowsUser, err))
		}
		defer wipeBytes(token)

		models, err := h.Store.Employees().Models(ctx, employee.ID)
		if err != nil {
			return Result{}, err
		}
		available, err := h.Catalog.Models(ctx)
		if err != nil {
			return Result{}, ClassError("gateway_catalog", err)
		}
		routable := intersect(available, models)
		if len(routable) == 0 {
			return Result{}, Permanent(fmt.Errorf(
				"none of %s's models are on the gateway; the machine would get a config pointing at nothing", employee.WindowsUser))
		}
		catalogJSON, err := catalog.Build(available, routable)
		if err != nil {
			return Result{}, Permanent(fmt.Errorf("build the model catalog: %w", err))
		}
		set[model.PathCodexConfig] = []byte(creds.RenderGatewayConfig(
			h.GatewayBaseURL, employee.WindowsUser, routable, string(token)))
		set[model.PathCodexModels] = catalogJSON
		allowed = len(routable)
	}

	if err := h.publish(ctx, employee, set); err != nil {
		return Result{}, err
	}
	if err := h.refreshBindings(ctx, employee); err != nil {
		return Result{}, err
	}
	return Result{Note: fmt.Sprintf("published %d model(s) to %s", allowed, employee.WindowsUser)}, nil
}

// publish merges into whatever archive is already there, minus the entries
// nobody publishes any more.
//
// The merge is what keeps an entry this export did not produce; the Claude
// entries an earlier console published are the exception, dropped on the way
// through so that an archive stops carrying a login for a tool that is no
// longer managed.
func (h OSSExport) publish(ctx context.Context, employee repo.Employee, set model.CredentialSet) error {
	key := ossclient.UserKey(employee.WindowsUser, "credentials.zip")
	existing := model.CredentialSet{}
	data, _, err := h.Objects.Get(key)
	switch {
	case err == nil:
		existing, err = creds.Unpack(data)
		if err != nil {
			// Leaving the archive alone is the safe failure: replacing one we
			// cannot read would throw away whatever else is in it.
			return Permanent(fmt.Errorf("the existing archive for %s cannot be read; it was left alone: %w",
				employee.WindowsUser, err))
		}
	case errors.Is(err, ossclient.ErrNotFound):
	default:
		return ClassError("oss_read", err)
	}

	merged := creds.Merge(existing, set)
	for _, legacy := range model.LegacyClaudeEntries {
		delete(merged, legacy)
	}
	blob, err := creds.Pack(merged)
	if err != nil {
		return Permanent(fmt.Errorf("pack credentials for %s: %w", employee.WindowsUser, err))
	}
	// The same bytes go to the bucket for agents that read objects and to
	// the table for agents that ask the console.
	sum := sha256.Sum256(blob)
	if err := h.Store.CredentialBundles().Put(ctx, employee.ID, employee.AuthEpoch, blob, hex.EncodeToString(sum[:16])); err != nil {
		return err
	}
	if err := h.Objects.Put(key, blob); err != nil {
		return ClassError("oss_write", err)
	}
	return nil
}

// withdraw removes the delivered credentials.
func (h OSSExport) withdraw(ctx context.Context, employee repo.Employee) error {
	if err := h.Store.CredentialBundles().Purge(ctx, employee.ID); err != nil {
		return err
	}
	if err := h.Objects.Delete(ossclient.UserKey(employee.WindowsUser, "credentials.zip")); err != nil {
		return ClassError("oss_delete", err)
	}
	return nil
}

// refreshBindings rewrites the binding object of every machine this employee
// holds, so that a Windows user name change or a restart request reaches them.
func (h OSSExport) refreshBindings(ctx context.Context, employee repo.Employee) error {
	bindings, err := h.Store.Bindings().OpenByEmployee(ctx, employee.ID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		// The same lock a device export takes: two exports must not
		// interleave on one binding object. Employee-then-device is the only
		// order anything takes these in, so there is no cycle.
		if err := h.Store.Lock(ctx, "oss_export:device:"+binding.DeviceID); err != nil {
			return err
		}
		if _, err := h.exportBinding(ctx, binding.DeviceID); err != nil {
			return err
		}
	}
	return nil
}

// intersect keeps the employee's models that the gateway actually routes,
// in the gateway's own order.
//
// A model retired from the gateway must not make the export impossible: that
// would strand the employee with no config at all over a model nobody uses any
// more. An empty result is still refused, because a config naming nothing is
// worse than no config.
func intersect(available []litellm.Model, wanted []string) []string {
	if len(wanted) == 0 {
		// No allowlist means everything the gateway offers, which is what an
		// account opened without a model selection gets.
		out := make([]string, 0, len(available))
		for _, m := range available {
			out = append(out, m.Name)
		}
		return out
	}
	want := make(map[string]bool, len(wanted))
	for _, name := range wanted {
		want[name] = true
	}
	out := make([]string, 0, len(wanted))
	for _, m := range available {
		if want[m.Name] {
			out = append(out, m.Name)
		}
	}
	return out
}

func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
