package worker

import (
	"context"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// CatalogSync keeps the employee-side models.json in step with LiteLLM.
//
// LiteLLM does not notify windows-control when an administrator hides or
// removes a deployment. The old flow only refreshed the picker when an
// operator pressed "全部下发" or changed a channel in this console, so a
// direct LiteLLM change left every Windows machine with the old catalog.
// This handler turns the gateway catalog into a periodic, idempotent
// observation. A changed snapshot queues the same OSS/API export used by a
// manual delivery, preserving one publication path.
type CatalogSync struct {
	Store   repo.Store
	Ops     *ops.Service
	Catalog ModelCatalog
}

func (h CatalogSync) Run(ctx context.Context, _ repo.Task) (Result, error) {
	if h.Ops == nil || h.Catalog == nil {
		return Result{Note: "catalog sync not configured"}, nil
	}

	available, err := h.Catalog.Models(ctx)
	if err != nil {
		return Result{}, ClassError("gateway_catalog", err)
	}
	channels, _, err := repo.LoadGatewayChannels(ctx, h.Store.Settings())
	if err != nil {
		return Result{}, err
	}
	available = litellm.WithoutChannels(available, channels.PausedSet())
	snapshot := ops.SnapshotOf(available)

	state, version, err := h.Ops.CatalogDeliveryState(ctx)
	if err != nil {
		return Result{}, err
	}
	if state.Fleet.Digest == snapshot.Digest {
		return Result{Note: fmt.Sprintf("catalog unchanged (%d model(s))", len(snapshot.Models))}, nil
	}

	// DeliverCatalog deliberately rejects an empty target set for a manual
	// button press. Automatic sync has a different responsibility: if all
	// models disappeared, it must withdraw the old credentials rather than
	// leave a stale picker on the desktop. Skip only when there is nobody to
	// receive credentials; a later onboarding will publish the current catalog.
	recipients, err := h.hasRecipients(ctx)
	if err != nil {
		return Result{}, err
	}
	if !recipients {
		return Result{Note: fmt.Sprintf("catalog changed to %d model(s); no active credential recipient", len(snapshot.Models))}, nil
	}

	queued, err := h.Ops.DeliverCatalog(ctx, snapshot, "", version, "system:catalog-sync", "catalog-sync:"+snapshot.Digest)
	if err != nil {
		return Result{}, err
	}
	note := fmt.Sprintf("catalog changed; queued %d employee refresh(es), now %d model(s)", queued, len(snapshot.Models))
	// An empty Fleet digest is the initial baseline, not a gateway change. For
	// later changes, keep a durable alert so configured notification channels
	// can tell the operator that the desktop catalogs are being refreshed.
	if state.Fleet.Digest != "" {
		if err := OpenCatalogChangedAlert(ctx, h.Store, state.Fleet.Models, snapshot.Models, snapshot.Digest, queued); err != nil {
			// Delivery already succeeded; a notification failure must not make the
			// catalog task retry and enqueue another delivery.
			note += "; notification record failed: " + err.Error()
		}
	}
	return Result{Note: note}, nil
}

func (h CatalogSync) hasRecipients(ctx context.Context) (bool, error) {
	employees, err := h.Store.Employees().List(ctx, repo.EmployeeFilter{})
	if err != nil {
		return false, err
	}
	for _, employee := range employees {
		if !employee.Active() {
			continue
		}
		_, err := h.Store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway)
		if err == nil {
			return true, nil
		}
		if err != repo.ErrNotFound {
			return false, err
		}
	}
	return false, nil
}

// OpenCatalogChangedAlert records one catalog transition. The digest is part
// of the fingerprint so two different gateway changes produce two notices,
// while a retry of the same task does not duplicate one.
func OpenCatalogChangedAlert(ctx context.Context, store repo.Store, before, after []string, digest string, queued int) error {
	_, _, err := store.Alerts().Open(ctx, repo.NewAlert{
		Kind:        repo.AlertCatalogChanged,
		Fingerprint: repo.AlertCatalogChanged + ":" + digest,
		Severity:    repo.SeverityWarn,
		SubjectType: "settings",
		SubjectID:   "model_catalog",
		Title:       "LiteLLM 模型目录已变化",
		Detail:      fmt.Sprintf("旧目录 %v；新目录 %v；已排队刷新 %d 位员工。", before, after, queued),
	})
	return err
}
