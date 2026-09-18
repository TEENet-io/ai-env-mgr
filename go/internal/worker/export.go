package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/catalog"
	"github.com/TEENet-io/ai-env-mgr/internal/creds"
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
	Keyring secrets.Keyring
	Catalog ModelCatalog
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
	switch {
	case payload.PolicyVersion > 0:
		return h.exportPolicy(ctx, payload.PolicyVersion)
	case payload.DeviceID != "":
		return h.exportBinding(ctx, payload.DeviceID)
	case payload.EmployeeID != "":
		return h.exportEmployee(ctx, payload.EmployeeID)
	default:
		return Result{}, Permanent(errors.New("the task says nothing about what to export"))
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
	if err := h.Objects.Put(ossclient.PolicyKey(), current.Content); err != nil {
		return Result{}, ClassError("oss_write", err)
	}
	note := fmt.Sprintf("policy version %d", current.Version)
	if current.Version != requested {
		note += fmt.Sprintf(" (the task asked for %d, which has been superseded)", requested)
	}
	return Result{Note: note}, nil
}

// exportBinding writes, or removes, one machine's binding object.
func (h OSSExport) exportBinding(ctx context.Context, deviceID string) (Result, error) {
	device, err := h.Store.Devices().ByID(ctx, deviceID)
	if errors.Is(err, repo.ErrNotFound) {
		return Result{}, Permanent(fmt.Errorf("machine %s no longer exists", deviceID))
	}
	if err != nil {
		return Result{}, err
	}

	binding, err := h.Store.Bindings().Open(ctx, device.ID)
	if errors.Is(err, repo.ErrNotFound) {
		// Nobody is assigned to it. The agent reads the absence as "not
		// assigned yet" and keeps applying the machine-wide policy, which is
		// exactly right for a machine that has just been taken back.
		if err := h.Objects.Delete(ossclient.BindingKey(device.Hostname)); err != nil {
			return Result{}, ClassError("oss_delete", err)
		}
		return Result{Note: "unbound"}, nil
	}
	if err != nil {
		return Result{}, err
	}

	employee, err := h.Store.Employees().ByID(ctx, binding.EmployeeID)
	if err != nil {
		return Result{}, err
	}
	object := model.Binding{
		User:    employee.WindowsUser,
		BoundAt: binding.BoundAt.UTC().Format(time.RFC3339),
		Note:    binding.Note,
	}
	// The one-shot Codex restart rides on the binding because it is the one
	// object every agent already reads every cycle.
	if binding.RestartNonce != "" {
		object.RestartCodex = binding.RestartNonce
		if binding.RestartAt != nil {
			object.RestartCodexAt = binding.RestartAt.UTC().Format(time.RFC3339)
		}
	}
	data, err := json.MarshalIndent(object, "", "  ")
	if err != nil {
		return Result{}, Permanent(fmt.Errorf("encode binding: %w", err))
	}
	if err := h.Objects.Put(ossclient.BindingKey(device.Hostname), data); err != nil {
		return Result{}, ClassError("oss_write", err)
	}
	return Result{Note: fmt.Sprintf("%s -> %s", device.Hostname, employee.WindowsUser)}, nil
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

	credential, err := h.Store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		if err := h.withdraw(ctx, employee); err != nil {
			return Result{}, err
		}
		if err := h.refreshBindings(ctx, employee); err != nil {
			return Result{}, err
		}
		return Result{Note: "credentials withdrawn"}, nil
	case err != nil:
		return Result{}, err
	}
	if !employee.Active() {
		// A live credential for somebody who has left is a state the database
		// should not be in; say so rather than publishing it.
		return Result{}, Permanent(fmt.Errorf(
			"employee %s is offboarded but still holds a live credential", employee.WindowsUser))
	}

	token, err := h.Keyring.Open(ctx, credential.Ciphertext, credential.KeyVersion,
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
	allowed := intersect(available, models)
	if len(allowed) == 0 {
		return Result{}, Permanent(fmt.Errorf(
			"none of %s's models are on the gateway; the machine would get a config pointing at nothing", employee.WindowsUser))
	}
	catalogJSON, err := catalog.Build(available, allowed)
	if err != nil {
		return Result{}, Permanent(fmt.Errorf("build the model catalog: %w", err))
	}

	set := model.CredentialSet{
		model.PathCodexConfig: []byte(creds.RenderGatewayConfig(
			h.GatewayBaseURL, employee.WindowsUser, allowed, string(token))),
		model.PathCodexModels: catalogJSON,
	}
	if err := h.publish(ctx, employee, set); err != nil {
		return Result{}, err
	}
	if err := h.refreshBindings(ctx, employee); err != nil {
		return Result{}, err
	}
	return Result{Note: fmt.Sprintf("published %d model(s) to %s", len(allowed), employee.WindowsUser)}, nil
}

// publish merges into whatever archive is already there.
//
// Codex and Claude credentials are published independently, so overwriting the
// archive outright would silently drop whichever tool was not part of this
// call -- and the employee would find one of their tools logged out.
func (h OSSExport) publish(_ context.Context, employee repo.Employee, set model.CredentialSet) error {
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

	blob, err := creds.Pack(creds.Merge(existing, set))
	if err != nil {
		return Permanent(fmt.Errorf("pack credentials for %s: %w", employee.WindowsUser, err))
	}
	if err := h.Objects.Put(key, blob); err != nil {
		return ClassError("oss_write", err)
	}
	return nil
}

// withdraw removes the delivered credentials.
func (h OSSExport) withdraw(_ context.Context, employee repo.Employee) error {
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
