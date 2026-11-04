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
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

// ModelCatalog is the gateway's list of routable models, used to build the
// picker's models.json.
type ModelCatalog interface {
	Models(ctx context.Context) ([]litellm.Model, error)
}

// OSSExport builds what the agents fetch from the device API and wakes the
// machines waiting for it.
//
// It once wrote the same things to OSS for agents that read the bucket; every
// machine talks to the console now, and the bucket holds only installers,
// collected data and backups. The name, and the task kind, stay because
// queued tasks carry them.
//
// It is written to be re-runnable at any moment: the export always reproduces
// the whole bundle from the current state rather than applying a change. A
// retry, a duplicate task, or a run that happens out of order therefore all
// converge on the same bytes.
type OSSExport struct {
	Store repo.Store
	// Notifier, when set, is woken after the export: this is the
	// authoritative wake, the one that follows every change, because every
	// change queues an export.
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

	// One export per subject at a time, across every worker: two of them
	// interleaved -- a second worker, or one that took over an expired lease
	// while the first was still writing -- could store the older bundle last.
	// The lock is held for the whole read-then-write.
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

// exportPolicy confirms there is a policy to serve; the run's notify then
// wakes every machine to fetch it from the device API.
//
// It reports whatever is current rather than the version the task names: two
// publishes in quick succession queue two exports, and both serve the newer.
func (h OSSExport) exportPolicy(ctx context.Context, requested int64) (Result, error) {
	current, err := h.Store.Policies().Current(ctx)
	if errors.Is(err, repo.ErrNotFound) {
		return Result{}, Permanent(errors.New("no policy has been published"))
	}
	if err != nil {
		return Result{}, err
	}
	note := fmt.Sprintf("policy version %d", current.Version)
	if current.Version != requested {
		note += fmt.Sprintf(" (the task asked for %d, which has been superseded)", requested)
	}
	return Result{Note: note}, nil
}

// exportBinding describes one machine's assignment, as the device API will
// serve it (deviceconfig), for the task's note; the run's notify then wakes
// the machine to fetch it.
func (h OSSExport) exportBinding(ctx context.Context, deviceID string) (Result, error) {
	cfg, err := deviceconfig.Build(ctx, h.Store, deviceID)
	if errors.Is(err, repo.ErrNotFound) {
		return Result{}, Permanent(fmt.Errorf("machine %s no longer exists", deviceID))
	}
	if err != nil {
		return Result{}, err
	}
	switch {
	case cfg.Forgotten:
		return Result{Note: "forgotten"}, nil
	case !cfg.HasBinding:
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
	return Result{Note: note}, nil
}

// exportEmployee stores, or removes, one employee's credential bundle, and
// refreshes the binding of every machine assigned to them.
//
// Removing is how a revocation reaches a desktop: the device API answers 404,
// the agent deletes the local credentials and ends the Codex session that
// still holds the token in memory.
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
	// arrived -- means no bundle. The agent reports the absence as "no
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
		// A paused channel's models leave the picker (设置在总览"渠道").
		channels, _, err := repo.LoadGatewayChannels(ctx, h.Store.Settings())
		if err != nil {
			return Result{}, err
		}
		available = litellm.WithoutChannels(available, channels.PausedSet())
		routable := intersect(available, models)
		if len(routable) == 0 {
			// A gateway model can be removed outside this console. Retaining the
			// old bundle would leave a stale model picker on the desktop, so
			// withdraw the published bundle when the employee's last allowed model
			// disappears. The stored gateway credential remains; if a model is
			// re-enabled later, the next catalog sync can publish it again.
			if err := h.withdraw(ctx, employee); err != nil {
				return Result{}, err
			}
			if err := h.refreshBindings(ctx, employee); err != nil {
				return Result{}, err
			}
			return Result{Note: fmt.Sprintf("credentials withdrawn; none of %s's models remain on the gateway", employee.WindowsUser)}, nil
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

// publish merges into the bundle already stored, minus the entries nobody
// publishes any more.
//
// The merge is what keeps an entry this export did not produce; the Claude
// entries an earlier console published are the exception, dropped on the way
// through so that a bundle stops carrying a login for a tool that is no
// longer managed.
func (h OSSExport) publish(ctx context.Context, employee repo.Employee, set model.CredentialSet) error {
	existing := model.CredentialSet{}
	data, _, err := h.Store.CredentialBundles().Live(ctx, employee.ID)
	switch {
	case err == nil:
		existing, err = creds.Unpack(data)
		if err != nil {
			// Leaving the bundle alone is the safe failure: replacing one we
			// cannot read would throw away whatever else is in it.
			return Permanent(fmt.Errorf("the existing bundle for %s cannot be read; it was left alone: %w",
				employee.WindowsUser, err))
		}
	case errors.Is(err, repo.ErrNotFound):
	default:
		return err
	}

	merged := creds.Merge(existing, set)
	for _, legacy := range model.LegacyClaudeEntries {
		delete(merged, legacy)
	}
	blob, err := creds.Pack(merged)
	if err != nil {
		return Permanent(fmt.Errorf("pack credentials for %s: %w", employee.WindowsUser, err))
	}
	sum := sha256.Sum256(blob)
	return h.Store.CredentialBundles().Put(ctx, employee.ID, employee.AuthEpoch, blob, hex.EncodeToString(sum[:16]))
}

// withdraw removes the delivered credentials.
func (h OSSExport) withdraw(ctx context.Context, employee repo.Employee) error {
	return h.Store.CredentialBundles().Purge(ctx, employee.ID)
}

// refreshBindings re-exports the binding of every machine this employee
// holds, so that a Windows user name change or a restart request reaches them.
func (h OSSExport) refreshBindings(ctx context.Context, employee repo.Employee) error {
	bindings, err := h.Store.Bindings().OpenByEmployee(ctx, employee.ID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		// The same lock a device export takes: two exports must not
		// interleave on one binding. Employee-then-device is the only
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
