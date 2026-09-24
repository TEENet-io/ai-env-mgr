// Package deviceconfig computes what one machine should be running: the
// fleet policy plus that machine's own binding -- who is assigned to it,
// which versions it is aimed at, the one-shot requests waiting for it.
//
// It is the single place this is worked out. The OSS exporter writes it
// into the bucket for agents that still read objects; the device API hands
// it to agents that ask the console directly. Two callers, one answer.
package deviceconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Config is one machine's complete configuration.
type Config struct {
	// ETag names this exact configuration; it changes whenever anything
	// below does. Clients compare it for equality and nothing else.
	ETag string `json:"etag"`

	// Policy is what every machine gets. HasPolicy is false before the
	// first publish: the machine-specific part still exists, the fleet
	// part is empty.
	Policy        model.Policy `json:"policy"`
	PolicyVersion int64        `json:"policyVersion"`
	HasPolicy     bool         `json:"hasPolicy"`

	// Binding is this machine's own part: assignee, targets, one-shot
	// requests. HasBinding is false when there is nothing machine-specific
	// at all -- unbound, no targets, nothing requested -- which the OSS
	// exporter renders as "no object" and the agent reads as "not assigned".
	Binding    model.Binding `json:"binding"`
	HasBinding bool          `json:"hasBinding"`
	// Bound is whether an employee is assigned; the name is in Binding.User.
	Bound bool `json:"bound"`

	// CredentialsETag identifies the credential bundle the machine should
	// hold for its assignee: empty when unbound or when no token has been
	// issued yet. A change here is what makes the agent fetch credentials.
	CredentialsETag string `json:"credentialsEtag"`

	// Forgotten is a machine an administrator removed from the console. The
	// agent should drop what it holds; the exporter deletes its objects.
	Forgotten bool `json:"forgotten,omitempty"`

	// Hostname is what the machine calls itself, for the exporter's keys and
	// the notes; not part of the ETag.
	Hostname string `json:"-"`
	// AssigneeEmployeeID is the assignee's id, for callers that need the
	// row rather than the Windows user; not part of the ETag.
	AssigneeEmployeeID string `json:"-"`
}

// Build computes the configuration for one device.
func Build(ctx context.Context, store repo.Store, deviceID string) (Config, error) {
	var cfg Config
	device, err := store.Devices().ByID(ctx, deviceID)
	if err != nil {
		return cfg, err
	}
	cfg.Hostname = device.Hostname
	if device.Status == repo.DeviceRevoked {
		cfg.Forgotten = true
		cfg.ETag = etagOf(cfg)
		return cfg, nil
	}

	current, err := store.Policies().Current(ctx)
	switch {
	case err == nil:
		if err := json.Unmarshal(current.Content, &cfg.Policy); err != nil {
			return cfg, fmt.Errorf("policy version %d is not readable: %w", current.Version, err)
		}
		cfg.PolicyVersion, cfg.HasPolicy = current.Version, true
	case errors.Is(err, repo.ErrNotFound):
	default:
		return cfg, err
	}

	if cfg.Binding, err = machineTargets(ctx, store, device.ID); err != nil {
		return cfg, err
	}
	binding, err := store.Bindings().Open(ctx, device.ID)
	unbound := errors.Is(err, repo.ErrNotFound)
	if err != nil && !unbound {
		return cfg, err
	}
	cfg.Binding.SyncRequested = device.SyncNonce
	if !unbound {
		employee, err := store.Employees().ByID(ctx, binding.EmployeeID)
		if err != nil {
			return cfg, err
		}
		cfg.Bound = true
		cfg.AssigneeEmployeeID = employee.ID
		cfg.Binding.User = employee.WindowsUser
		cfg.Binding.BoundAt = binding.BoundAt.UTC().Format(time.RFC3339)
		cfg.Binding.Note = binding.Note
		// The one-shot Codex restart rides on the binding because it is the
		// one object every agent already reads every cycle.
		if binding.RestartNonce != "" {
			cfg.Binding.RestartCodex = binding.RestartNonce
			if binding.RestartAt != nil {
				cfg.Binding.RestartCodexAt = binding.RestartAt.UTC().Format(time.RFC3339)
			}
		}
		if employee.Active() {
			credential, err := store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway)
			switch {
			case err == nil:
				// The bundle's own etag when there is one: a change of models
				// rewrites the bundle but not the token, and it is this value
				// changing that ends the agent's wait. The token id stands in
				// only until a bundle has been built.
				cfg.CredentialsETag = credential.ID
				if etag, err := store.CredentialBundles().LiveETag(ctx, employee.ID); err == nil {
					cfg.CredentialsETag = etag
				} else if !errors.Is(err, repo.ErrNotFound) {
					return cfg, err
				}
			case errors.Is(err, repo.ErrNotFound):
			default:
				return cfg, err
			}
		}
	}
	cfg.HasBinding = cfg.Bound || cfg.Binding.AgentTarget != nil || cfg.Binding.CodexTarget != nil || device.SyncNonce != ""
	cfg.ETag = etagOf(cfg)
	return cfg, nil
}

// machineTargets is what one machine should be running of each product, as
// binding fields. Three answers per product, and only the first says nothing:
//
//   - no target ever: the field is absent and the fleet policy applies;
//   - an open target in a paused rollout: a target with an empty version --
//     "this machine: nothing" -- so that pausing holds the machine where it
//     is instead of handing it back to the fleet target;
//   - an open target, or none open but one that succeeded: that version, so
//     that a machine the rollout updated stays updated when the rollout is
//     over, rather than sliding back to an older fleet target.
func machineTargets(ctx context.Context, store repo.Store, deviceID string) (model.Binding, error) {
	var out model.Binding
	for _, product := range []string{repo.ProductAgent, repo.ProductCodex} {
		target, err := store.Releases().OpenTarget(ctx, deviceID, product)
		if errors.Is(err, repo.ErrNotFound) {
			target, err = store.Releases().LastSucceededTarget(ctx, deviceID, product)
			if errors.Is(err, repo.ErrNotFound) {
				continue
			}
		}
		if err != nil {
			return out, err
		}
		rt := &model.ReleaseTarget{Generation: target.Generation}
		rollout, err := store.Releases().RolloutByID(ctx, target.RolloutID)
		if err != nil {
			return out, err
		}
		if !(target.Status == repo.TargetPending && rollout.PausedAt != nil) {
			artifact, err := store.Releases().ArtifactByID(ctx, target.ArtifactID)
			if err != nil {
				return out, err
			}
			rt.Version, rt.SHA256, rt.Key = artifact.Version, artifact.SHA256, artifact.ObjectKey
		}
		if product == repo.ProductAgent {
			out.AgentTarget = rt
		} else {
			out.CodexTarget = rt
		}
	}
	return out, nil
}

// etagOf hashes everything a client acts on. The hostname and the assignee
// id are left out: neither changes what the machine does.
func etagOf(cfg Config) string {
	cfg.ETag = ""
	data, _ := json.Marshal(cfg)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// BindingObject renders the binding as the OSS exporter writes it, so the
// object an old agent reads and the configuration a new one fetches say
// the same thing.
func (c Config) BindingObject() ([]byte, error) {
	return json.MarshalIndent(c.Binding, "", "  ")
}
