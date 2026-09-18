package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Policy edits. Each one reads the current policy, changes one thing,
// validates, and publishes -- inside one transaction, so two administrators
// editing different fields at the same moment cannot lose each other's change
// to a read-modify-write race.
//
// The rules are the ones the old console enforced, kept here because
// policy.json is not a trust boundary: the agent validates too, but a value no
// agent understands should never reach the fleet in the first place.

// CurrentPolicy returns the fleet policy, or the built-in default when nothing
// has been published yet. Any other failure is an error: a read that failed
// must not become "the fleet has no policy".
func (s *Service) CurrentPolicy(ctx context.Context) (model.Policy, int64, error) {
	return currentPolicy(ctx, s.store)
}

func currentPolicy(ctx context.Context, store repo.Store) (model.Policy, int64, error) {
	current, err := store.Policies().Current(ctx)
	if errors.Is(err, repo.ErrNotFound) {
		return model.DefaultPolicy(), 0, nil
	}
	if err != nil {
		return model.Policy{}, 0, err
	}
	var p model.Policy
	if err := json.Unmarshal(current.Content, &p); err != nil {
		return model.Policy{}, 0, fmt.Errorf("policy version %d is not readable: %w", current.Version, err)
	}
	return p, current.Version, nil
}

// mutatePolicy applies fn to the current policy and publishes the result.
func (s *Service) mutatePolicy(ctx context.Context, note, actor, requestID string, fn func(*model.Policy) error) (model.Policy, error) {
	var result model.Policy
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		before, beforeVersion, err := currentPolicy(ctx, tx)
		if err != nil {
			return err
		}
		after := before
		if err := fn(&after); err != nil {
			return err
		}
		after.UpdatedAt = s.now().Format(time.RFC3339)
		content, err := json.MarshalIndent(after, "", "  ")
		if err != nil {
			return fmt.Errorf("encode policy: %w", err)
		}
		published, err := s.publishPolicyTx(ctx, tx, content, note, actor, requestID, beforeVersion, before, after)
		if err != nil {
			return err
		}
		_ = published
		result = after
		return nil
	})
	if err != nil {
		return model.Policy{}, fmt.Errorf("%s: %w", note, err)
	}
	return result, nil
}

// MutateDomains adds and removes blocked domains. The set is deduplicated and
// sorted so the stored list is stable regardless of call order.
func (s *Service) MutateDomains(ctx context.Context, add, remove []string, actor, requestID string) (model.Policy, error) {
	return s.mutatePolicy(ctx, "edit blocked domains", actor, requestID, func(p *model.Policy) error {
		set := map[string]struct{}{}
		for _, d := range p.BlockedDomains {
			if n := normalizeDomain(d); n != "" {
				set[n] = struct{}{}
			}
		}
		for _, d := range add {
			if n := normalizeDomain(d); n != "" {
				set[n] = struct{}{}
			}
		}
		for _, d := range remove {
			delete(set, normalizeDomain(d))
		}
		domains := make([]string, 0, len(set))
		for d := range set {
			domains = append(domains, d)
		}
		sort.Strings(domains)
		p.BlockedDomains = domains
		return nil
	})
}

func normalizeDomain(d string) string { return strings.ToLower(strings.TrimSpace(d)) }

// MutateAppLockerAllowPaths edits the directories every user may execute
// from. An invalid addition aborts the whole change: a half-applied allow list
// is the drift the validation exists to stop. An existing entry that no longer
// validates is dropped on the way past -- one unrelated edit is a chance to
// heal a list written before the rules tightened.
func (s *Service) MutateAppLockerAllowPaths(ctx context.Context, add, remove []string, actor, requestID string) (model.Policy, error) {
	return s.mutatePolicy(ctx, "edit AppLocker allow paths", actor, requestID, func(p *model.Policy) error {
		for _, a := range add {
			if err := model.ValidateAppLockerPath(strings.TrimSpace(a)); err != nil {
				return fmt.Errorf("allow path %q: %w", a, err)
			}
		}
		var existing []string
		for _, e := range p.AppLockerAllowPaths {
			if err := model.ValidateAppLockerPath(strings.TrimSpace(e)); err == nil {
				existing = append(existing, e)
			}
		}
		drop := map[string]bool{}
		for _, r := range remove {
			drop[strings.ToLower(strings.TrimSpace(r))] = true
		}
		var keep []string
		for _, e := range append(append([]string{}, existing...), add...) {
			if !drop[strings.ToLower(strings.TrimSpace(e))] {
				keep = append(keep, e)
			}
		}
		p.AppLockerAllowPaths = model.NormalizeAppLockerPaths(keep)
		// Each rule is a rule on every employee machine, and a few tool
		// directories is what this exists for.
		if n := len(p.AppLockerAllowPaths); n > model.AppLockerMaxAllowPaths {
			return fmt.Errorf("%d allow paths: at most %d are allowed", n, model.AppLockerMaxAllowPaths)
		}
		return nil
	})
}

// SetAppLockerMode holds every machine's rule collections in one enforcement
// mode. "audit" is the off switch for testing; "" hands the mode back to the
// image. Nothing here deletes a rule.
func (s *Service) SetAppLockerMode(ctx context.Context, mode, actor, requestID string) (model.Policy, error) {
	if err := model.ValidateAppLockerMode(mode); err != nil {
		return model.Policy{}, err
	}
	return s.mutatePolicy(ctx, "set AppLocker mode", actor, requestID, func(p *model.Policy) error {
		p.AppLockerMode = mode
		return nil
	})
}

// SetBlockEnabled flips the site block for the whole fleet.
func (s *Service) SetBlockEnabled(ctx context.Context, enabled bool, actor, requestID string) (model.Policy, error) {
	return s.mutatePolicy(ctx, "set site blocking", actor, requestID, func(p *model.Policy) error {
		p.BlockEnabled = enabled
		return nil
	})
}

// SetSyncInterval changes how often agents sync. It is always clamped: a bad
// interval can strand a machine, and a non-positive one would sync
// continuously.
func (s *Service) SetSyncInterval(ctx context.Context, minutes int, actor, requestID string) (model.Policy, error) {
	return s.mutatePolicy(ctx, "set sync interval", actor, requestID, func(p *model.Policy) error {
		p.SyncIntervalMinutes = model.ClampInterval(minutes, p.SyncIntervalMinutes)
		return nil
	})
}

// SetCollect turns session collection on or off. since and quiet are
// pointers so the switch can be flipped without restating options set
// earlier.
func (s *Service) SetCollect(ctx context.Context, enabled bool, since *string, quiet *int, actor, requestID string) (model.Policy, error) {
	return s.mutatePolicy(ctx, "set session collection", actor, requestID, func(p *model.Policy) error {
		p.CollectEnabled = enabled
		if since != nil {
			p.CollectSince = *since
		}
		if quiet != nil {
			p.CollectQuietSeconds = *quiet
		}
		return nil
	})
}

// SetAgentUpdate points the fleet at a new agent binary that the caller has
// already uploaded. version empty is the kill switch.
//
// The upload is not done here: it is a large file with a progress bar, and
// this is a transaction. The order that matters is upload first, then this --
// pointing agents at a binary that is not there yet is a fleet of agents
// retrying a download.
func (s *Service) SetAgentUpdate(ctx context.Context, version, sha256hex, actor, requestID string) (model.Policy, error) {
	if version != "" && len(sha256hex) != 64 {
		return model.Policy{}, errors.New("an agent update needs the binary's SHA-256")
	}
	return s.mutatePolicy(ctx, "set agent update target", actor, requestID, func(p *model.Policy) error {
		p.AgentUpdateVersion = version
		p.AgentUpdateSHA256 = sha256hex
		return nil
	})
}

// SetCodexUpdate points the fleet at a Codex installer already uploaded to
// objectKey. version empty is the kill switch.
func (s *Service) SetCodexUpdate(ctx context.Context, version, sha256hex, objectKey, actor, requestID string) (model.Policy, error) {
	if version != "" && (len(sha256hex) != 64 || objectKey == "") {
		return model.Policy{}, errors.New("a Codex update needs the installer's SHA-256 and its object key")
	}
	return s.mutatePolicy(ctx, "set Codex update target", actor, requestID, func(p *model.Policy) error {
		p.CodexVersion = version
		p.CodexSHA256 = sha256hex
		p.CodexKey = objectKey
		// Agents 1.2.5 through 1.2.7 gate the install on this and treat a
		// missing or zero value as "not my turn"; newer agents ignore it.
		p.CodexRolloutPct = 100
		return nil
	})
}
