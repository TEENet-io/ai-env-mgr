package agentcore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentapi"
	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// fakeConsole is enough of the device API for a full cycle.
type fakeConsole struct {
	mu       sync.Mutex
	cfg      deviceconfig.Config
	creds    []byte
	credsTag string
	statuses [][]byte
	logs     [][]byte
	uploads  map[string][]byte
	calls    []string
	artifact []byte
	srv      *httptest.Server
}

func newFakeConsole(t *testing.T) *fakeConsole {
	t.Helper()
	f := &fakeConsole{uploads: map[string][]byte{}}
	f.cfg = deviceconfig.Config{ETag: "e1", HasPolicy: true, Policy: model.Policy{BlockEnabled: true, BlockedDomains: []string{"openai.com"}, SyncIntervalMinutes: 60}}
	mux := http.NewServeMux()
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "unauthorized", 401)
				return
			}
			f.mu.Lock()
			f.calls = append(f.calls, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
			next(w, r)
		}
	}
	mux.HandleFunc("GET /agent/v1/config", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("If-None-Match") == f.cfg.ETag {
			w.WriteHeader(304)
			return
		}
		json.NewEncoder(w).Encode(f.cfg)
	}))
	mux.HandleFunc("GET /agent/v1/wait", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		cfg := f.cfg
		f.mu.Unlock()
		if r.URL.Query().Get("etag") == cfg.ETag {
			w.WriteHeader(204)
			return
		}
		json.NewEncoder(w).Encode(cfg)
	}))
	mux.HandleFunc("GET /agent/v1/credentials", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.creds == nil {
			http.Error(w, "none", 404)
			return
		}
		if r.Header.Get("If-None-Match") == f.credsTag {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", f.credsTag)
		w.Write(f.creds)
	}))
	mux.HandleFunc("POST /agent/v1/status", authed(func(w http.ResponseWriter, r *http.Request) {
		var buf strings.Builder
		b := make([]byte, 1<<20)
		n, _ := r.Body.Read(b)
		buf.Write(b[:n])
		f.mu.Lock()
		f.statuses = append(f.statuses, []byte(buf.String()))
		f.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /agent/v1/log", authed(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		f.mu.Lock()
		f.logs = append(f.logs, b[:n])
		f.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /agent/v1/artifact/{product}/{version}", authed(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.srv.URL+"/blob/"+r.PathValue("product"), 302)
	}))
	mux.HandleFunc("GET /blob/{product}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "the bucket must never see the token", 400)
			return
		}
		w.Write(f.artifact)
	})
	mux.HandleFunc("POST /agent/v1/collect/upload-url", authed(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ User, Rel string }
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]string{"url": f.srv.URL + "/put/" + req.User + "/" + req.Rel, "contentType": "application/octet-stream"})
	}))
	mux.HandleFunc("PUT /put/{user}/{rel...}", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<20)
		n, _ := r.Body.Read(b)
		f.mu.Lock()
		f.uploads[r.PathValue("user")+"/"+r.PathValue("rel")] = b[:n]
		f.mu.Unlock()
		w.WriteHeader(200)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func apiSyncer(t *testing.T, console *fakeConsole, app *fakeApplier) (*Syncer, *APISource) {
	t.Helper()
	src := NewAPISource(&agentapi.Client{BaseURL: console.srv.URL, Token: "tok", Version: "1.3.0", HTTP: console.srv.Client()})
	src.Download = console.srv.Client()
	s := &Syncer{
		Source: src, Applier: app,
		Machine: &fakeMachine{name: "DESKTOP-A", localUsers: []string{"Administrator", "work1"}, profileDir: t.TempDir()},
		Version: "1.3.0", StateDir: t.TempDir(), FallbackInterval: 30,
	}
	return s, src
}

func TestAPISourceDrivesAFullCycle(t *testing.T) {
	console := newFakeConsole(t)
	app := &fakeApplier{}
	s, src := apiSyncer(t, console, app)

	// Unbound: policy applied, status posted, no credentials asked for.
	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(app.applied) != 1 || !app.applied[0].BlockEnabled || len(console.statuses) != 1 || st.SyncIntervalMinutes != 60 {
		t.Fatalf("first cycle: applied=%d statuses=%d interval=%d", len(app.applied), len(console.statuses), st.SyncIntervalMinutes)
	}
	if st.BoundUser != "" || !strings.Contains(strings.Join(st.Warnings, " "), "no binding") {
		t.Fatalf("unbound status = %+v", st)
	}
	for _, c := range console.calls {
		if strings.Contains(c, "credentials") {
			t.Fatal("an unbound machine must not ask for credentials")
		}
	}

	// Bound, with a bundle: it is delivered once and then not fetched again.
	set := model.CredentialSet{model.PathCodexConfig: []byte("base_url = \"x\"")}
	zip, _ := creds.Pack(set)
	console.mu.Lock()
	console.cfg = deviceconfig.Config{ETag: "e2", HasPolicy: true, HasBinding: true, Bound: true,
		Policy: console.cfg.Policy, Binding: model.Binding{User: "work1", BoundAt: "now"}, CredentialsETag: "c1"}
	console.creds, console.credsTag = zip, "c1"
	console.mu.Unlock()
	st, _ = s.RunOnce()
	if st.BoundUser != "work1" || !st.CredsApplied || len(app.deployed) != 1 {
		t.Fatalf("bound cycle = %+v deployed=%d", st, len(app.deployed))
	}
	// The policy did not change between cycles: the registry was not rewritten.
	if len(app.applied) != 1 {
		t.Fatalf("policy applied %d times; an unchanged policy is applied once", len(app.applied))
	}
	before := len(console.calls)
	st, _ = s.RunOnce()
	if !st.CredsApplied || len(app.deployed) != 1 {
		t.Fatalf("third cycle re-deployed: %+v", st)
	}
	fetched := 0
	for _, c := range console.calls[before:] {
		if strings.HasSuffix(c, "/credentials") {
			fetched++
		}
	}
	if fetched != 1 {
		t.Fatalf("credentials asked %d times in one cycle (one If-None-Match check expected)", fetched)
	}

	// Wait: nothing changed → false; a change → true and the new document
	// is what the next cycle uses without asking again.
	if changed, err := src.Wait(context.Background()); err != nil || changed {
		t.Fatalf("quiet wait: %v %v", changed, err)
	}
	console.mu.Lock()
	console.cfg.ETag = "e3"
	console.cfg.Policy.BlockEnabled = false
	console.mu.Unlock()
	if changed, err := src.Wait(context.Background()); err != nil || !changed {
		t.Fatalf("changed wait: %v %v", changed, err)
	}
	before = len(console.calls)
	s.RunOnce()
	if len(app.applied) != 2 || app.applied[1].BlockEnabled {
		t.Fatalf("the policy from the wait was applied: %+v", app.applied)
	}

	// Log upload and a collected file both go through the console.
	if err := s.UploadLog([]byte("tail")); err != nil || len(console.logs) != 1 {
		t.Fatalf("log: %v %d", err, len(console.logs))
	}
	if err := src.Put("agent_workdir/work1/data_collect/2026/09/a.jsonl", []byte("session")); err != nil {
		t.Fatal(err)
	}
	if string(console.uploads["work1/2026/09/a.jsonl"]) != "session" {
		t.Fatalf("uploads = %v", console.uploads)
	}

	// Artifacts: bytes and to-file, via the signed link, never with the token.
	console.artifact = []byte("installer bytes")
	target := model.ReleaseTarget{Version: "0.42.0"}
	if data, err := src.ArtifactBytes(ProductAgent, target); err != nil || string(data) != "installer bytes" {
		t.Fatalf("artifact bytes: %q %v", data, err)
	}
	dest := filepath.Join(t.TempDir(), "codex-setup.exe")
	if sum, err := src.ArtifactToFile(ProductCodex, target, dest); err != nil || len(sum) != 64 {
		t.Fatalf("artifact to file: %s %v", sum, err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "installer bytes" {
		t.Fatal("the file is not what was served")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("the partial file was left behind")
	}

	// A refused token is remembered, and cleared by Reset.
	src.Client.Token = "revoked"
	if _, err := s.RunOnce(); err != nil {
		t.Fatal("a refused token is reported in the status, not as a cycle failure")
	}
	if !src.NeedsEnrol() {
		t.Fatal("a 401 must mark the source as needing enrolment")
	}
	src.Reset("tok")
	if src.NeedsEnrol() || src.ETag() != "" {
		t.Fatal("reset clears the mark and the cached document")
	}
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	_ = time.Second
}
