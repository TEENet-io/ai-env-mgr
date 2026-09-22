package deviceapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// handleArtifact hands out a download link for a version this machine is
// meant to install: its own target or the fleet's. Anything else is 403 --
// the version library is not a public download site.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request, device repo.Device) {
	if s.Objects == nil {
		http.Error(w, "downloads are not available", http.StatusServiceUnavailable)
		return
	}
	product, version := r.PathValue("product"), r.PathValue("version")
	if product != repo.ProductAgent && product != repo.ProductCodex {
		http.Error(w, "unknown product", http.StatusNotFound)
		return
	}
	cfg, err := deviceconfig.Build(r.Context(), s.Store, device.ID)
	if err != nil {
		s.fail(w, r, "build config", err)
		return
	}
	allowed := map[string]bool{}
	if product == repo.ProductAgent {
		allowed[cfg.Policy.AgentUpdateVersion] = true
		if cfg.Binding.AgentTarget != nil {
			allowed[cfg.Binding.AgentTarget.Version] = true
		}
	} else {
		allowed[cfg.Policy.CodexVersion] = true
		if cfg.Binding.CodexTarget != nil {
			allowed[cfg.Binding.CodexTarget.Version] = true
		}
	}
	delete(allowed, "")
	if !allowed[version] {
		http.Error(w, "this machine is not aimed at that version", http.StatusForbidden)
		return
	}
	key, sha := "", ""
	artifact, err := s.Store.Releases().ArtifactByVersion(r.Context(), product, version)
	switch {
	case err == nil:
		key, sha = artifact.ObjectKey, artifact.SHA256
	case errors.Is(err, repo.ErrNotFound):
		// A version set before the version library existed lives at the
		// fixed key the fleet policy names, as it always did for agents
		// reading the bucket.
		switch {
		case product == repo.ProductAgent && version == cfg.Policy.AgentUpdateVersion:
			key, sha = ossclient.AgentBinaryKey(), cfg.Policy.AgentUpdateSHA256
		case product == repo.ProductCodex && version == cfg.Policy.CodexVersion && cfg.Policy.CodexKey != "":
			key, sha = cfg.Policy.CodexKey, cfg.Policy.CodexSHA256
		default:
			http.Error(w, "no such version", http.StatusNotFound)
			return
		}
	default:
		s.fail(w, r, "read artifact", err)
		return
	}
	link, err := s.Objects.SignedURL(key, artifactTTL)
	if err != nil {
		s.fail(w, r, "sign download", err)
		return
	}
	w.Header().Set("X-Artifact-SHA256", sha)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, link, http.StatusFound)
}

type uploadRequest struct {
	User string `json:"user"`
	Rel  string `json:"rel"`
}

// handleCollectURL signs an upload for one session file of the machine's
// assignee. The key is fixed by the signature; the caller chooses only the
// file name under that user's directory.
func (s *Server) handleCollectURL(w http.ResponseWriter, r *http.Request, device repo.Device) {
	if s.Objects == nil {
		http.Error(w, "uploads are not available", http.StatusServiceUnavailable)
		return
	}
	var req uploadRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Rel = strings.TrimSpace(req.Rel)
	if req.User == "" || req.Rel == "" || strings.HasPrefix(req.Rel, "/") || strings.Contains(req.Rel, "..") || strings.Contains(req.Rel, "\\") {
		http.Error(w, "a user and a relative file path are required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	binding, err := s.Store.Bindings().Open(ctx, device.ID)
	if errors.Is(err, repo.ErrNotFound) {
		http.Error(w, "this machine is not assigned to anybody", http.StatusForbidden)
		return
	}
	if err != nil {
		s.fail(w, r, "read binding", err)
		return
	}
	employee, err := s.Store.Employees().ByID(ctx, binding.EmployeeID)
	if err != nil {
		s.fail(w, r, "read employee", err)
		return
	}
	if !strings.EqualFold(employee.WindowsUser, req.User) {
		http.Error(w, "this machine may only upload for its own user", http.StatusForbidden)
		return
	}
	link, err := s.Objects.SignedPutURL(ossclient.DataCollectKey(employee.WindowsUser, req.Rel), uploadTTL, "application/octet-stream")
	if err != nil {
		s.fail(w, r, "sign upload", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": link, "contentType": "application/octet-stream"})
}

func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request, device repo.Device) {
	token, err := s.Store.DeviceTokens().Rotate(r.Context(), device.ID, s.TokenGrace)
	if err != nil {
		s.fail(w, r, "rotate token", err)
		return
	}
	s.event("info", "token rotated", device, nil)
	writeJSON(w, http.StatusOK, map[string]string{"deviceToken": token})
}
