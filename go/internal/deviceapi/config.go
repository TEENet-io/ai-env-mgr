package deviceapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func (s *Server) config(ctx context.Context, device repo.Device) (deviceconfig.Config, error) {
	cfg, err := deviceconfig.Build(ctx, s.Store, device.ID)
	if err != nil {
		return cfg, err
	}
	if s.Bucket != nil && !cfg.Forgotten {
		data, _, err := s.Bucket.Get(ossclient.MachineApplicationsKey(device.Hostname))
		switch {
		case err == nil:
			var apps model.MachineApplications
			if err := json.Unmarshal(data, &apps); err != nil {
				return cfg, err
			}
			cfg.Applications = &apps
			cfg.RecomputeETag()
		case errors.Is(err, ossclient.ErrNotFound):
			// No plan is the normal state for machines without software tasks.
		case err != nil:
			return cfg, err
		}
	}
	return cfg, nil
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, device repo.Device) {
	cfg, err := s.config(r.Context(), device)
	if err != nil {
		s.fail(w, r, "build config", err)
		return
	}
	if r.Header.Get("If-None-Match") == cfg.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", cfg.ETag)
	writeJSON(w, http.StatusOK, cfg)
}

// handleWait holds the request until the configuration differs from the
// etag the agent has, or WaitMax passes. A second poll from the same
// machine ends the first one at once: an agent that reconnected must not
// have two of these open.
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request, device repo.Device) {
	have := r.URL.Query().Get("etag")
	ctx, cancel := context.WithTimeout(r.Context(), s.WaitMax)
	defer cancel()
	p := &poll{cancel: cancel}
	s.takeOver(device.ID, p)
	defer s.release(device.ID, p)

	ticker := time.NewTicker(s.Recheck)
	defer ticker.Stop()
	for {
		// Register for the wake before reading, so a change that lands
		// between the two is not missed until the recheck.
		mine, all := s.Hub.Wait(device.ID)
		cfg, err := s.config(ctx, device)
		if err != nil {
			if ctx.Err() != nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			s.fail(w, r, "build config", err)
			return
		}
		if cfg.ETag != have {
			w.Header().Set("ETag", cfg.ETag)
			writeJSON(w, http.StatusOK, cfg)
			return
		}
		select {
		case <-mine:
		case <-all:
		case <-ticker.C:
		case <-ctx.Done():
			// Time is up, or a newer poll took over: nothing changed.
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
}
