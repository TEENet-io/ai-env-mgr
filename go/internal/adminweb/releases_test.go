package adminweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestStagePackageWritesTheUploadToSpoolAndHashesIt(t *testing.T) {
	spool := t.TempDir()
	payload := bytes.Repeat([]byte("agent"), 10_000)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("version", "1.2.16")
	fw, _ := mw.CreateFormFile("file", "agent.exe")
	fw.Write(payload)
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/releases/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if err := parseUpload(r); err != nil {
		t.Fatal(err)
	}

	stage, err := stagePackage(r, spool, maxPackageBytes)
	if err != nil {
		t.Fatalf("stagePackage: %v", err)
	}
	staged, err := stage(nil)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer staged.Remove()
	sum := sha256.Sum256(payload)
	if staged.SHA256 != hex.EncodeToString(sum[:]) || staged.Size != int64(len(payload)) || staged.Source != "agent.exe" {
		t.Fatalf("staged = %+v", staged)
	}
	if !strings.HasPrefix(staged.Path, spool) {
		t.Fatalf("staged outside the spool: %s", staged.Path)
	}
	info, _ := os.Stat(staged.Path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spool file mode %v, want 0600", info.Mode().Perm())
	}
	staged.Remove()
	if _, err := os.Stat(staged.Path); !os.IsNotExist(err) {
		t.Fatal("Remove must delete the spool file")
	}
}

func TestStagePackageFromURLNeverBuffersTheBody(t *testing.T) {
	spool := t.TempDir()
	served := bytes.Repeat([]byte("x"), 3<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(served)
	}))
	defer srv.Close()

	r := httptest.NewRequest(http.MethodPost, "/releases/upload", strings.NewReader("url="+url.QueryEscape(srv.URL+"/codex.exe")+"&version=0.42.0"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	parseUpload(r)
	stage, err := stagePackage(r, spool, 1<<20) // a limit below the body
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stage(nil); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversize download must be refused, err = %v", err)
	}
	entries, _ := os.ReadDir(spool)
	if len(entries) != 0 {
		t.Fatalf("a refused download left %d file(s) in the spool", len(entries))
	}
}

func TestReleasesPageListsCandidatesAndAimsOnlyOnRequest(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	a, err := s.dbm.ops.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.42.0",
		SHA256: strings.Repeat("c", 64), SizeBytes: 700 << 20, ObjectKey: "agent_workdir/_codex/codex-setup-0.42.0.exe", CreatedBy: "t"}, "t", "r")
	if err != nil {
		t.Fatal(err)
	}
	page := dbGet(t, h, "/releases", cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "0.42.0") || !strings.Contains(page.Body.String(), "未设置") {
		t.Fatalf("releases page: %d", page.Code)
	}
	csrf := csrfFrom(t, s, cookie, "/releases")
	rec := dbPost(t, h, "/releases/global", url.Values{"csrf": {csrf}, "product": {"codex"}, "version": {"0.42.0"}, "confirm": {"0.42.0"}}, cookie)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("global: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	pol, _, _ := s.dbm.ops.CurrentPolicy(ctx)
	if pol.CodexVersion != "0.42.0" || pol.CodexSHA256 != a.SHA256 {
		t.Fatalf("policy = %+v", pol)
	}
	page = dbGet(t, h, "/releases", cookie)
	if !strings.Contains(page.Body.String(), "全局目标</span>") {
		t.Fatal("the page does not mark the fleet target")
	}
	// The old fleet-wide publish routes lead here in the database mode.
	for _, old := range []string{"/rollout", "/agent/publish", "/codex/publish"} {
		if rec := dbGet(t, h, old, cookie); rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound {
			t.Fatalf("%s should redirect, got %d", old, rec.Code)
		}
	}
}

func TestUploadingACandidateRegistersItWithoutAimingAnyone(t *testing.T) {
	s, fs := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	csrf := csrfFrom(t, s, cookie, "/releases")
	payload := bytes.Repeat([]byte("agent"), 2048)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("csrf", csrf)
	mw.WriteField("product", "agent")
	mw.WriteField("version", "1.2.16")
	mw.WriteField("notes", "no Claude")
	fw, _ := mw.CreateFormFile("file", "agent.exe")
	fw.Write(payload)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/releases/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("upload: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if !s.jobs.wait(10e9) {
		t.Fatal("the upload job did not finish")
	}
	if snap := s.jobs.snapshot(); snap.Err != "" {
		t.Fatalf("job failed: %s", snap.Err)
	}
	a, err := s.dbm.store.Releases().ArtifactByVersion(t.Context(), repo.ProductAgent, "1.2.16")
	if err != nil {
		t.Fatalf("not registered: %v", err)
	}
	sum := sha256.Sum256(payload)
	if a.SHA256 != hex.EncodeToString(sum[:]) || a.Notes != "no Claude" || a.Status != repo.ArtifactCandidate {
		t.Fatalf("artifact = %+v", a)
	}
	if got, ok := fs.objects[a.ObjectKey]; !ok || !bytes.Equal(got, payload) {
		t.Fatalf("the package is not at %s", a.ObjectKey)
	}
	if pol, _, _ := s.dbm.ops.CurrentPolicy(t.Context()); pol.AgentUpdateVersion != "" {
		t.Fatal("uploading a candidate must not aim the fleet")
	}
	if _, ok := fs.objects["agent_workdir/_agent/agent.exe"]; ok {
		t.Fatal("uploading a candidate must not touch the fixed key")
	}
	entries, _ := os.ReadDir(s.dbm.spool)
	if len(entries) != 0 {
		t.Fatalf("spool not cleaned: %d file(s)", len(entries))
	}
}
