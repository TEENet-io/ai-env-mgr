package deviceapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/status"
	"github.com/TEENet-io/ai-env-mgr/internal/worker"
)

// handleStatus takes the same report the agent used to write to the bucket.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, device repo.Device) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	st, err := status.Parse(raw)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(st.Machine), device.Hostname) {
		http.Error(w, "the report names another machine", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(raw)
	report := worker.ReportFromStatus(device.ID, st, raw, "api:"+hex.EncodeToString(sum[:8]))
	ctx := r.Context()
	if _, err := s.Store.Reports().Import(ctx, report); err != nil {
		s.fail(w, r, "import status", err)
		return
	}
	if err := worker.SettleTargets(ctx, s.Store, device.ID, st); err != nil {
		s.fail(w, r, "settle targets", err)
		return
	}
	seen := s.now()
	if report.LastSyncAt != nil {
		seen = *report.LastSyncAt
	}
	if err := s.Store.Devices().MarkSeen(ctx, device.ID, st.AgentVersion, seen); err != nil {
		s.fail(w, r, "mark seen", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request, device repo.Device) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogTail+1))
	if err != nil || len(raw) > maxLogTail {
		http.Error(w, "the log tail is limited to 64 KB", http.StatusRequestEntityTooLarge)
		return
	}
	if err := s.Store.Devices().SetLogTail(r.Context(), device.ID, string(raw), s.now()); err != nil {
		s.fail(w, r, "store log", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCredentials serves the bundle for the machine's assignee. An
// unbound machine, or one whose assignee has no token yet, gets 404: the
// agent reads that exactly as it read a missing object.
func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request, device repo.Device) {
	ctx := r.Context()
	binding, err := s.Store.Bindings().Open(ctx, device.ID)
	if errors.Is(err, repo.ErrNotFound) {
		http.Error(w, "not assigned", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, "read binding", err)
		return
	}
	zip, etag, err := s.Store.CredentialBundles().Live(ctx, binding.EmployeeID)
	if errors.Is(err, repo.ErrNotFound) {
		// A bundle delivered before the table existed is still in the
		// bucket, while the bucket is written. Serve that rather than
		// "nothing published", which the agent would act on by revoking.
		zip, etag, err = s.bundleFromBucket(ctx, binding.EmployeeID)
	}
	if errors.Is(err, repo.ErrNotFound) {
		http.Error(w, "no credentials published yet", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, "read bundle", err)
		return
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(zip)
}

// bundleFromBucket reads the employee's credentials.zip as the exporter
// wrote it to the bucket, for a machine whose assignee has no bundle row
// yet. ErrNotFound when the bucket is not read, not written, or has none.
func (s *Server) bundleFromBucket(ctx context.Context, employeeID string) ([]byte, string, error) {
	if s.Bucket == nil {
		return nil, "", repo.ErrNotFound
	}
	settings, _, err := repo.LoadDeviceChannelSettings(ctx, s.Store.Settings())
	if err != nil {
		return nil, "", err
	}
	if !settings.WriteOSSObjects {
		return nil, "", repo.ErrNotFound
	}
	employee, err := s.Store.Employees().ByID(ctx, employeeID)
	if err != nil {
		return nil, "", err
	}
	data, etag, err := s.Bucket.Get(ossclient.UserKey(employee.WindowsUser, "credentials.zip"))
	if errors.Is(err, ossclient.ErrNotFound) {
		return nil, "", repo.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if etag == "" {
		sum := sha256.Sum256(data)
		etag = "oss:" + hex.EncodeToString(sum[:8])
	}
	return data, etag, nil
}
