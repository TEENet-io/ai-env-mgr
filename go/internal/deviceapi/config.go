package deviceapi

import (
	"context"
	"net/http"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, device repo.Device) {
	cfg, err := deviceconfig.Build(r.Context(), s.Store, device.ID)
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
		cfg, err := deviceconfig.Build(r.Context(), s.Store, device.ID)
		if err != nil {
			s.fail(w, r, "build config", err)
			return
		}
		if cfg.ETag != have {
			w.Header().Set("ETag", cfg.ETag)
			writeJSON(w, http.StatusOK, cfg)
			return
		}
		mine, all := s.Hub.Wait(device.ID)
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
