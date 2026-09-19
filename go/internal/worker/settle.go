package worker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// settleTargets turns a machine's report into a result for its open
// targets. Success is the machine running the version, reported after the
// target was made; failure is the machine saying it tried this generation
// and could not. Anything else -- deferred, downloading, an older report --
// leaves the target pending. Nothing here guesses.
func settleTargets(ctx context.Context, store repo.Store, deviceID string, s model.Status) error {
	reportedAt, err := time.Parse(time.RFC3339, s.LastSync)
	if err != nil {
		return nil // a report with no usable time cannot settle anything
	}
	kinds := []struct {
		product    string
		running    string // what the machine has
		target     string // what it says it is aiming at
		generation int
		state      string
		prefix     string // the error line that explains a failure
	}{
		{repo.ProductAgent, s.AgentVersion, s.AgentUpdateTarget, s.AgentUpdateGeneration, s.AgentUpdateState, "update:"},
		{repo.ProductCodex, s.CodexVersion, s.CodexTarget, s.CodexTargetGeneration, s.CodexState, "codex:"},
	}
	for _, k := range kinds {
		target, err := store.Releases().OpenTarget(ctx, deviceID, k.product)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !reportedAt.After(target.CreatedAt) {
			continue
		}
		artifact, err := store.Releases().ArtifactByID(ctx, target.ArtifactID)
		if err != nil {
			return err
		}
		switch {
		case k.running == artifact.Version:
			_, err = store.Releases().FinishTarget(ctx, target.ID, repo.TargetSucceeded, "", k.running)
		case k.target == artifact.Version && k.generation == target.Generation &&
			(k.state == agentcore.CodexFailed || k.state == agentcore.AgentUpdateFailed):
			_, err = store.Releases().FinishTarget(ctx, target.ID, repo.TargetFailed, firstError(s.Errors, k.prefix), k.running)
		}
		if err != nil && !errors.Is(err, repo.ErrConflict) {
			return err
		}
	}
	return nil
}

// firstError picks the line about this product, or a generic note.
func firstError(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return "the machine reported failure without a matching error line"
}
