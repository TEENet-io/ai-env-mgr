package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Reconcile asks the gateway what it actually holds and writes the answer
// down.
//
// It exists because every other path records an intention. A provisioning call
// that times out leaves us genuinely unsure whether the token was created; a
// revoke that fails after the third retry leaves a key we believe is gone.
// Neither can be settled by trying again -- repeating an uncertain write is
// how you end up with two of something -- and both are settled by looking.
//
// The case that matters is a grant we consider revoked which the gateway still
// serves: a working token nobody thinks exists. Nothing else in the system can
// find that.
type Reconcile struct {
	Store   repo.Store
	Gateway Gateway
	// StaleAfter is how long an observation stays good enough. Everything
	// checked longer ago than this is looked at again.
	StaleAfter time.Duration
	// Limit bounds one pass, so a fleet-sized backlog is worked through over
	// several runs instead of holding a lease for an hour.
	Limit int
	// OnDrift is called for each grant whose observed state did not match what
	// the console believed. It is the hook the alerting hangs from; nothing is
	// "fixed" silently.
	OnDrift func(grant repo.Grant, observed string)
}

// Run checks the grants that are due and records what the gateway holds.
func (h Reconcile) Run(ctx context.Context, _ repo.Task) (Result, error) {
	staleAfter := h.StaleAfter
	if staleAfter <= 0 {
		staleAfter = time.Hour
	}
	limit := h.Limit
	if limit <= 0 {
		limit = 100
	}

	grants, err := h.Store.Grants().NeedsReconcile(ctx, "", staleAfter, limit)
	if err != nil {
		return Result{}, err
	}

	checked, drifted, repaired := 0, 0, 0
	for _, grant := range grants {
		key, found, err := h.Gateway.FindKeyByAlias(ctx, grant.KeyAlias)
		if err != nil {
			// Stop rather than carry on: if the gateway is answering with
			// errors, the remaining answers would be no better, and recording
			// "missing" for a gateway that is merely unwell would revoke
			// nothing and alarm everybody.
			return Result{Note: fmt.Sprintf("checked %d before the gateway stopped answering", checked)},
				gatewayError("find_key", err)
		}
		checked++

		observed := repo.ActualMissing
		if found {
			observed = repo.ActualActive
		}

		switch {
		case grant.Desired == repo.GrantRevoked && found:
			// The one that matters. We believe this token is gone and the
			// gateway is still serving it.
			if err := h.Gateway.DeleteKeyByAlias(ctx, grant.KeyAlias); err != nil {
				return Result{}, gatewayError("delete_key", err)
			}
			observed = repo.ActualRevoked
			drifted++
			repaired++
			h.drift(grant, "still live on the gateway")
		case grant.Desired == repo.GrantActive && !found:
			// The other direction: we believe somebody has a working token and
			// they do not. This is not repaired here -- issuing a token is a
			// provisioning decision with an epoch attached, and doing it from
			// a reconciliation would mint credentials nobody asked for.
			drifted++
			h.drift(grant, "missing from the gateway")
		case grant.Desired == repo.GrantRevoked && !found:
			observed = repo.ActualMissing
		}

		reason := ""
		if grant.Desired == repo.GrantActive && !found {
			reason = "the gateway does not have this key"
		}
		if _, err := h.Store.Grants().RecordActual(ctx, grant.ID, observed, reason); err != nil {
			return Result{}, err
		}
		_ = key
	}

	note := fmt.Sprintf("checked %d grant(s)", checked)
	if drifted > 0 {
		note += fmt.Sprintf(", %d did not match (%d repaired)", drifted, repaired)
	}
	return Result{Note: note}, nil
}

func (h Reconcile) drift(grant repo.Grant, observed string) {
	if h.OnDrift != nil {
		h.OnDrift(grant, observed)
	}
}

// EnqueueReconcile puts a reconciliation on the queue for the hour at holds.
// The scheduler does this on its own; it is exported for a console button.
func EnqueueReconcile(ctx context.Context, store repo.Store, at time.Time) error {
	return enqueuePeriodic(ctx, store, repo.TaskReconcile, at, EveryReconcile, 3)
}
