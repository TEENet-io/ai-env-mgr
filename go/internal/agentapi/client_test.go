package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// fakeConsole is the device API as the client expects it.
func fakeConsole(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	cfg := deviceconfig.Config{ETag: "e1", HasPolicy: true, Policy: model.Policy{BlockEnabled: true}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/v1/enrol", func(w http.ResponseWriter, r *http.Request) {
		var req enrolRequest
		json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, "enrol "+req.Hostname+" "+req.AgentVersion+" "+r.Header.Get("User-Agent"))
		if req.Hostname == "TAKEN" {
			http.Error(w, "already", http.StatusConflict)
			return
		}
		json.NewEncoder(w).Encode(enrolResponse{DeviceID: "d1", DeviceToken: "tok-1"})
	})
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok-1" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			seen = append(seen, r.Method+" "+r.URL.RequestURI()+" v="+r.Header.Get("X-Agent-Version"))
			next(w, r)
		}
	}
	mux.HandleFunc("GET /agent/v1/config", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == cfg.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", cfg.ETag)
		json.NewEncoder(w).Encode(cfg)
	}))
	mux.HandleFunc("GET /agent/v1/wait", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("etag") == cfg.ETag {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(cfg)
	}))
	mux.HandleFunc("GET /agent/v1/credentials", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "c1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "c1")
		w.Write([]byte("PK-zip"))
	}))
	mux.HandleFunc("POST /agent/v1/status", authed(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	mux.HandleFunc("POST /agent/v1/log", authed(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	mux.HandleFunc("GET /agent/v1/artifact/{product}/{version}", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("version") != "0.42.0" {
			http.Error(w, "not aimed", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Artifact-SHA256", strings.Repeat("a", 64))
		http.Redirect(w, r, "https://bucket.example/codex-0.42.0.exe?sig=x", http.StatusFound)
	}))
	mux.HandleFunc("GET /agent/v1/application-tasks/{id}/manifest", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "install-1" || r.Header.Get("X-Application-Lease-Token") != "lease-1" {
			http.Error(w, "bad lease", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(model.Application{AppID: "editor", Version: "1.0", InstallerType: "msi", Enabled: true, Approved: true})
	}))
	mux.HandleFunc("GET /agent/v1/application-tasks/{id}/download", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "install-1" || r.Header.Get("X-Application-Lease-Token") != "lease-1" {
			http.Error(w, "bad lease", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Artifact-SHA256", strings.Repeat("b", 64))
		http.Redirect(w, r, "https://bucket.example/editor-1.0.msi?sig=task", http.StatusFound)
	}))
	mux.HandleFunc("POST /agent/v1/collect/upload-url", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"url": "https://bucket.example/put?sig=y", "contentType": "application/octet-stream"})
	}))
	mux.HandleFunc("POST /agent/v1/token/rotate", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"deviceToken": "tok-2"})
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestClientSpeaksTheDeviceAPI(t *testing.T) {
	srv, seen := fakeConsole(t)
	ctx := context.Background()
	id, token, err := Enrol(ctx, srv.URL, "PC-1", "1.3.0", srv.Client())
	if err != nil || id != "d1" || token != "tok-1" {
		t.Fatalf("enrol: %s %s %v", id, token, err)
	}
	if _, _, err := Enrol(ctx, srv.URL, "TAKEN", "1.3.0", srv.Client()); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("taken hostname: %v", err)
	}
	c := &Client{BaseURL: srv.URL + "/", Token: token, Version: "1.3.0", HTTP: srv.Client()}

	cfg, unchanged, err := c.Config(ctx, "")
	if err != nil || unchanged || cfg == nil || cfg.ETag != "e1" || !cfg.Policy.BlockEnabled {
		t.Fatalf("config: %+v %v %v", cfg, unchanged, err)
	}
	if _, unchanged, err := c.Config(ctx, "e1"); err != nil || !unchanged {
		t.Fatalf("config unchanged: %v %v", unchanged, err)
	}
	if _, changed, err := c.Wait(ctx, "e1"); err != nil || changed {
		t.Fatalf("quiet wait: %v %v", changed, err)
	}
	if got, changed, err := c.Wait(ctx, "old"); err != nil || !changed || got.ETag != "e1" {
		t.Fatalf("changed wait: %+v %v %v", got, changed, err)
	}
	data, etag, exists, unchanged, err := c.Credentials(ctx, "")
	if err != nil || !exists || unchanged || string(data) != "PK-zip" || etag != "c1" {
		t.Fatalf("credentials: %q %s %v %v %v", data, etag, exists, unchanged, err)
	}
	if _, etag, exists, unchanged, err := c.Credentials(ctx, "c1"); err != nil || !exists || !unchanged || etag != "c1" {
		t.Fatalf("credentials unchanged: %s %v %v %v", etag, exists, unchanged, err)
	}
	if err := c.PostStatus(ctx, []byte(`{"machine":"PC-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.PostLog(ctx, []byte("log")); err != nil {
		t.Fatal(err)
	}
	link, sha, err := c.ArtifactURL(ctx, "codex", "0.42.0")
	if err != nil || !strings.HasPrefix(link, "https://bucket.example/codex-0.42.0.exe") || sha != strings.Repeat("a", 64) {
		t.Fatalf("artifact: %s %s %v", link, sha, err)
	}
	if _, _, err := c.ArtifactURL(ctx, "codex", "0.43.0"); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a refused artifact is a plain error: %v", err)
	}
	manifest, err := c.ApplicationTaskManifest(ctx, "install-1", "lease-1")
	if err != nil || manifest.AppID != "editor" || manifest.Version != "1.0" {
		t.Fatalf("task manifest: %+v %v", manifest, err)
	}
	taskLink, taskSHA, err := c.ApplicationTaskURL(ctx, "install-1", "lease-1")
	if err != nil || !strings.Contains(taskLink, "sig=task") || taskSHA != strings.Repeat("b", 64) {
		t.Fatalf("task download: %s %s %v", taskLink, taskSHA, err)
	}
	if _, err := c.ApplicationTaskManifest(ctx, "install-1", ""); err == nil {
		t.Fatal("empty task lease must be rejected before making a request")
	}
	put, ct, err := c.CollectUploadURL(ctx, "work1", "2026/09/a.jsonl")
	if err != nil || !strings.Contains(put, "sig=y") || ct != "application/octet-stream" {
		t.Fatalf("upload url: %s %s %v", put, ct, err)
	}
	if next, err := c.Rotate(ctx); err != nil || next != "tok-2" {
		t.Fatalf("rotate: %s %v", next, err)
	}
	// A refused token is the one error the agent acts on differently.
	bad := &Client{BaseURL: srv.URL, Token: "nope", HTTP: srv.Client()}
	if _, _, err := bad.Config(ctx, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	if _, _, err := bad.Wait(ctx, "x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad token wait: %v", err)
	}
	// Every authenticated call carried the version and the token.
	for _, line := range *seen {
		if strings.HasPrefix(line, "GET") || strings.HasPrefix(line, "POST /agent/v1/status") {
			if !strings.HasSuffix(line, "v=1.3.0") {
				t.Fatalf("missing version: %s", line)
			}
		}
	}
	if !strings.Contains((*seen)[0], "enrol PC-1 1.3.0 ai-env-agent/1.3.0") {
		t.Fatalf("enrol line = %s", (*seen)[0])
	}
}

func TestTokenStoreRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := LoadToken(dir); !errors.Is(err, ErrNoToken) {
		t.Fatalf("before saving: %v", err)
	}
	if err := SaveToken(dir, "tok-1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, tokenFile))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("token file mode: %v %v", info.Mode(), err)
	}
	if got, err := LoadToken(dir); err != nil || got != "tok-1" {
		t.Fatalf("load: %q %v", got, err)
	}
	if err := SaveToken(dir, "tok-2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadToken(dir); got != "tok-2" {
		t.Fatalf("replaced: %q", got)
	}
	if err := ForgetToken(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(dir); !errors.Is(err, ErrNoToken) {
		t.Fatalf("after forgetting: %v", err)
	}
	if err := ForgetToken(dir); err != nil {
		t.Fatal("forgetting twice is fine")
	}
}
