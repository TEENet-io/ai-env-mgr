package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// RegisterArtifact records a package that is already in the bucket. It aims
// nothing at anybody: a candidate is a row, not a rollout.
//
// The same bytes under the same version is the same artifact, returned as it
// is, so a retried upload converges. Different bytes under a used version is
// refused: the version name is the promise.
func (s *Service) RegisterArtifact(ctx context.Context, a repo.NewArtifact, actor, requestID string) (repo.Artifact, error) {
	if a.Product != repo.ProductAgent && a.Product != repo.ProductCodex {
		return repo.Artifact{}, fmt.Errorf("unknown product %q", a.Product)
	}
	if strings.TrimSpace(a.Version) == "" || len(a.SHA256) != 64 || a.SizeBytes <= 0 || a.ObjectKey == "" {
		return repo.Artifact{}, errors.New("an artifact needs a version, its SHA-256, its size and its object key")
	}
	var result repo.Artifact
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		existing, err := tx.Releases().ArtifactByVersion(ctx, a.Product, a.Version)
		switch {
		case err == nil:
			if !strings.EqualFold(existing.SHA256, a.SHA256) {
				return fmt.Errorf("%s %s already names a different package (sha256 %s…); a rebuild needs a new version",
					a.Product, a.Version, existing.SHA256[:12])
			}
			result = existing
			return nil
		case !errors.Is(err, repo.ErrNotFound):
			return err
		}
		created, err := tx.Releases().CreateArtifact(ctx, a)
		if err != nil {
			return err
		}
		result = created
		return s.auditTarget(ctx, tx, actor, requestID, ActionArtifactRegister, "artifact", created.ID, nil,
			map[string]any{"product": a.Product, "version": a.Version, "sha256": a.SHA256, "size": a.SizeBytes, "source": a.Source})
	})
	if err != nil {
		return repo.Artifact{}, fmt.Errorf("register %s %s: %w", a.Product, a.Version, err)
	}
	return result, nil
}

// SetArtifactStatus records acceptance, promotion to stable, or retirement.
func (s *Service) SetArtifactStatus(ctx context.Context, id string, status repo.ArtifactStatus, note, actor, requestID string) (repo.Artifact, error) {
	var result repo.Artifact
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		before, err := tx.Releases().ArtifactByID(ctx, id)
		if err != nil {
			return err
		}
		after, err := tx.Releases().SetArtifactStatus(ctx, id, status, note, actor)
		if err != nil {
			return err
		}
		result = after
		return s.auditTarget(ctx, tx, actor, requestID, ActionArtifactStatus, "artifact", id,
			map[string]any{"status": before.Status}, map[string]any{"status": after.Status, "note": note})
	})
	if err != nil {
		return repo.Artifact{}, fmt.Errorf("set artifact status: %w", err)
	}
	return result, nil
}

// SetGlobalTarget points every machine that reads the fleet policy at a
// registered artifact. This is the old channel, and stays the only channel
// for agents older than 1.2.16. The checksum and key come from the artifact
// row, never from the form.
func (s *Service) SetGlobalTarget(ctx context.Context, product, version, actor, requestID string) (model.Policy, error) {
	artifact, err := s.store.Releases().ArtifactByVersion(ctx, product, version)
	if err != nil {
		return model.Policy{}, fmt.Errorf("set global %s target: %w", product, err)
	}
	if artifact.Status == repo.ArtifactRetired {
		return model.Policy{}, fmt.Errorf("set global %s target: %s is retired", product, version)
	}
	pol, err := s.mutatePolicy(ctx, "set global "+product+" target", actor, requestID, func(p *model.Policy) error {
		switch product {
		case repo.ProductAgent:
			p.AgentUpdateVersion, p.AgentUpdateSHA256 = artifact.Version, artifact.SHA256
		case repo.ProductCodex:
			p.CodexVersion, p.CodexSHA256, p.CodexKey = artifact.Version, artifact.SHA256, artifact.ObjectKey
			p.CodexRolloutPct = 100 // agents 1.2.5-1.2.7 gate on it; see SetCodexUpdate
		default:
			return fmt.Errorf("unknown product %q", product)
		}
		return nil
	})
	if err != nil {
		return model.Policy{}, err
	}
	return pol, nil
}

// ClearGlobalTarget is the kill switch for the old channel.
func (s *Service) ClearGlobalTarget(ctx context.Context, product, actor, requestID string) (model.Policy, error) {
	return s.mutatePolicy(ctx, "clear global "+product+" target", actor, requestID, func(p *model.Policy) error {
		switch product {
		case repo.ProductAgent:
			p.AgentUpdateVersion, p.AgentUpdateSHA256 = "", ""
		case repo.ProductCodex:
			p.CodexVersion, p.CodexSHA256, p.CodexKey, p.CodexRolloutPct = "", "", "", 0
		default:
			return fmt.Errorf("unknown product %q", product)
		}
		return nil
	})
}

// RolloutSpec is one decision: this artifact, these machines.
type RolloutSpec struct {
	Product, ArtifactID string
	DeviceIDs           []string
	Kind                repo.RolloutKind
	RollbackOf          string
	Note                string
	Actor, RequestID    string
}

// CreateRollout opens one target per chosen machine and queues each machine's
// binding export. It is refused whole if any machine cannot take a target:
// a rollout that silently skipped a machine would be reported as complete
// with that machine never updated.
func (s *Service) CreateRollout(ctx context.Context, spec RolloutSpec) (repo.Rollout, error) {
	if len(spec.DeviceIDs) == 0 {
		return repo.Rollout{}, errors.New("create rollout: choose at least one machine")
	}
	if spec.Kind == "" {
		spec.Kind = repo.RolloutRelease
	}
	var result repo.Rollout
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		artifact, err := tx.Releases().ArtifactByID(ctx, spec.ArtifactID)
		if err != nil {
			return err
		}
		if artifact.Product != spec.Product {
			return fmt.Errorf("artifact %s is a %s package, not %s", artifact.Version, artifact.Product, spec.Product)
		}
		if artifact.Status == repo.ArtifactRetired {
			return fmt.Errorf("%s %s is retired", artifact.Product, artifact.Version)
		}
		rollout, err := tx.Releases().CreateRollout(ctx, repo.NewRollout{
			Product: spec.Product, ArtifactID: artifact.ID, Kind: spec.Kind,
			RollbackOf: spec.RollbackOf, Note: spec.Note, CreatedBy: spec.Actor,
		})
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, id := range spec.DeviceIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			device, err := tx.Devices().ByID(ctx, id)
			if err != nil {
				return err
			}
			if device.Status == repo.DeviceRevoked {
				return fmt.Errorf("%s is revoked", device.Hostname)
			}
			if !model.AgentCanTakeTargets(device.AgentVersion) {
				return fmt.Errorf("%s runs agent %q, which only reads the fleet policy; targets need %s or later",
					device.Hostname, device.AgentVersion, model.MinTargetAgentVersion)
			}
			if artifact.MinAgentVersion != "" && model.CompareVersions(device.AgentVersion, artifact.MinAgentVersion) < 0 {
				return fmt.Errorf("%s runs agent %s; %s %s needs %s or later",
					device.Hostname, device.AgentVersion, artifact.Product, artifact.Version, artifact.MinAgentVersion)
			}
			target, err := tx.Releases().CreateTarget(ctx, device.ID, spec.Product, artifact.ID, rollout.ID)
			if err != nil {
				return err
			}
			if err := s.enqueueDeviceExport(ctx, tx, device, "target:"+target.ID); err != nil {
				return err
			}
			if err := s.auditTarget(ctx, tx, spec.Actor, spec.RequestID, ActionRolloutCreate, "device", device.ID, nil,
				map[string]any{"rollout": rollout.ID, "product": spec.Product, "version": artifact.Version,
					"generation": target.Generation, "kind": spec.Kind}); err != nil {
				return err
			}
		}
		result = rollout
		return nil
	})
	if err != nil {
		return repo.Rollout{}, fmt.Errorf("create rollout: %w", err)
	}
	s.wake(spec.DeviceIDs...)
	return result, nil
}

// SetRolloutPaused stops, or resumes, machines that have not started. The
// export omits the target of a paused rollout, so a machine that reads its
// binding sees nothing to do; a machine already installing carries on and
// reports.
func (s *Service) SetRolloutPaused(ctx context.Context, rolloutID string, paused bool, actor, requestID string) error {
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		rollout, err := tx.Releases().SetRolloutPaused(ctx, rolloutID, paused, actor)
		if err != nil {
			return err
		}
		marker := fmt.Sprintf("pause:%s:%t:%d", rollout.ID, paused, s.now().UnixNano())
		if err := s.reexportPending(ctx, tx, rollout.ID, marker); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionRolloutPause, "rollout", rollout.ID, nil,
			map[string]any{"paused": paused})
	})
	if err != nil {
		return fmt.Errorf("pause rollout: %w", err)
	}
	s.wakeRollout(ctx, rolloutID)
	return nil
}

// wakeRollout wakes every machine a rollout names.
func (s *Service) wakeRollout(ctx context.Context, rolloutID string) {
	if s.Notifier == nil {
		return
	}
	targets, err := s.store.Releases().TargetsByRollout(ctx, rolloutID)
	if err != nil {
		return
	}
	ids := make([]string, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.DeviceID)
	}
	s.wake(ids...)
}

// CancelRollout closes every pending target. Finished ones keep their result.
func (s *Service) CancelRollout(ctx context.Context, rolloutID, actor, requestID string) (int, error) {
	var n int
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		targets, err := tx.Releases().TargetsByRollout(ctx, rolloutID)
		if err != nil {
			return err
		}
		n, err = tx.Releases().CancelPending(ctx, rolloutID)
		if err != nil {
			return err
		}
		for _, t := range targets {
			if t.Status != repo.TargetPending {
				continue
			}
			device, err := tx.Devices().ByID(ctx, t.DeviceID)
			if err != nil {
				return err
			}
			if err := s.enqueueDeviceExport(ctx, tx, device, "cancel:"+t.ID); err != nil {
				return err
			}
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionRolloutCancel, "rollout", rolloutID, nil,
			map[string]any{"cancelled": n})
	})
	if err != nil {
		return 0, fmt.Errorf("cancel rollout: %w", err)
	}
	s.wakeRollout(ctx, rolloutID)
	return n, nil
}

// ExcludeTarget takes one machine out of a rollout, with a reason that goes
// into the report: excluded is neither success nor failure.
func (s *Service) ExcludeTarget(ctx context.Context, targetID, reason, actor, requestID string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("exclude machine: a reason is required")
	}
	var deviceID string
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		target, err := tx.Releases().FinishTarget(ctx, targetID, repo.TargetExcluded, reason, "")
		if err != nil {
			return err
		}
		deviceID = target.DeviceID
		device, err := tx.Devices().ByID(ctx, target.DeviceID)
		if err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device, "exclude:"+target.ID); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionTargetExclude, "device", device.ID, nil,
			map[string]any{"rollout": target.RolloutID, "target": target.ID, "reason": reason})
	})
	if err != nil {
		return fmt.Errorf("exclude machine: %w", err)
	}
	s.wake(deviceID)
	return nil
}

// RetryTarget opens a new generation for a machine whose target failed. The
// agent keys its one-attempt marker on version and generation, so a new
// generation is what makes it try the same version again.
func (s *Service) RetryTarget(ctx context.Context, targetID, actor, requestID string) (repo.Target, error) {
	var result repo.Target
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		failed, err := tx.Releases().TargetByID(ctx, targetID)
		if err != nil {
			return err
		}
		if failed.Status != repo.TargetFailed {
			return fmt.Errorf("only a failed target is retried; this one is %s", failed.Status)
		}
		device, err := tx.Devices().ByID(ctx, failed.DeviceID)
		if err != nil {
			return err
		}
		next, err := tx.Releases().CreateTarget(ctx, device.ID, failed.Product, failed.ArtifactID, failed.RolloutID)
		if err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device, "target:"+next.ID); err != nil {
			return err
		}
		result = next
		return s.auditTarget(ctx, tx, actor, requestID, ActionTargetRetry, "device", device.ID,
			map[string]any{"target": failed.ID, "generation": failed.Generation, "error": failed.ResultNote},
			map[string]any{"target": next.ID, "generation": next.Generation})
	})
	if err != nil {
		return repo.Target{}, fmt.Errorf("retry target: %w", err)
	}
	s.wake(result.DeviceID)
	return result, nil
}

// reexportPending queues a binding export for every machine still pending
// in a rollout.
func (s *Service) reexportPending(ctx context.Context, tx repo.Store, rolloutID, marker string) error {
	targets, err := tx.Releases().TargetsByRollout(ctx, rolloutID)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if t.Status != repo.TargetPending {
			continue
		}
		device, err := tx.Devices().ByID(ctx, t.DeviceID)
		if err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device, marker+":"+device.ID); err != nil {
			return err
		}
	}
	return nil
}
