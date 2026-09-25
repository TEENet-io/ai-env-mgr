package deviceapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

const applicationLease = 90 * time.Second

func (s *Server) handleApplicationTaskClaim(w http.ResponseWriter, r *http.Request, device repo.Device) {
	t, err := s.Store.ApplicationTasks().Claim(r.Context(), device.ID, applicationLease)
	if errors.Is(err, repo.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.fail(w, r, "claim application task", err)
		return
	}
	writeJSON(w, http.StatusOK, model.ApplicationTask{ID: t.ID, AppID: t.AppID, Version: t.Version, LeaseToken: t.LeaseToken, Attempts: t.Attempts, AllowDowngrade: t.AllowDowngrade})
}

type applicationTaskUpdate struct {
	LeaseToken string `json:"leaseToken"`
	Progress   string `json:"progress"`
	State      string `json:"state"`
	LastError  string `json:"lastError"`
}

func readApplicationTaskUpdate(w http.ResponseWriter, r *http.Request) (applicationTaskUpdate, bool) {
	var in applicationTaskUpdate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.LeaseToken == "" {
		http.Error(w, "invalid task update", http.StatusBadRequest)
		return in, false
	}
	return in, true
}

func (s *Server) handleApplicationTaskRenew(w http.ResponseWriter, r *http.Request, device repo.Device) {
	in, ok := readApplicationTaskUpdate(w, r)
	if !ok {
		return
	}
	if len(in.Progress) > 100 {
		http.Error(w, "progress too long", http.StatusBadRequest)
		return
	}
	_, err := s.Store.ApplicationTasks().Renew(r.Context(), r.PathValue("id"), device.ID, in.LeaseToken, in.Progress, applicationLease)
	if errors.Is(err, repo.ErrNotFound) {
		http.Error(w, "task cancelled or lease lost", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, r, "renew application task", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleApplicationTaskFinish(w http.ResponseWriter, r *http.Request, device repo.Device) {
	in, ok := readApplicationTaskUpdate(w, r)
	if !ok {
		return
	}
	if len(in.LastError) > 2048 {
		http.Error(w, "error too long", http.StatusBadRequest)
		return
	}
	_, err := s.Store.ApplicationTasks().Finish(r.Context(), r.PathValue("id"), device.ID, in.LeaseToken, in.State, in.LastError)
	if errors.Is(err, repo.ErrNotFound) {
		http.Error(w, "task cancelled or lease lost", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, r, "finish application task", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
