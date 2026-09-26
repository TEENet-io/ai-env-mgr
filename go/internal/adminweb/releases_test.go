package adminweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

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
	if !strings.Contains(page.Body.String(), `tag-ok">全局</span>`) {
		t.Fatal("the page does not mark the fleet target")
	}
	// The old fleet-wide publish routes lead here in the database mode.
	for _, old := range []string{"/rollout", "/agent/publish", "/codex/publish"} {
		if rec := dbGet(t, h, old, cookie); rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound {
			t.Fatalf("%s should redirect, got %d", old, rec.Code)
		}
	}
}

func TestRegisteringWhatCIUploadedReadsItOnceAndTouchesNothing(t *testing.T) {
	s, fs := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	// An empty library renders no form, so the token comes from a page that
	// always has one.
	csrf := csrfFrom(t, s, cookie, "/users")
	payload := bytes.Repeat([]byte("codex"), 4096)
	fs.objects["agent_workdir/_codex/codex-setup-0.42.0.exe"] = payload
	before := len(fs.objects)

	// Nothing there yet for 0.43.0: refused before any job starts.
	rec := dbPost(t, h, "/releases/register", url.Values{"csrf": {csrf}, "product": {"codex"}, "version": {"0.43.0"}}, cookie)
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("registering a version CI has not uploaded must be refused")
	}
	rec = dbPost(t, h, "/releases/register", url.Values{"csrf": {csrf}, "product": {"codex"}, "version": {"0.42.0"}, "min_agent": {"1.2.16"}, "notes": {"from CI"}}, cookie)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("register: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if !s.jobs.wait(10e9) {
		t.Fatal("the job did not finish")
	}
	if snap := s.jobs.snapshot(); snap.Err != "" {
		t.Fatalf("job failed: %s", snap.Err)
	}
	a, err := s.dbm.store.Releases().ArtifactByVersion(t.Context(), repo.ProductCodex, "0.42.0")
	if err != nil {
		t.Fatalf("not registered: %v", err)
	}
	sum := sha256.Sum256(payload)
	if a.SHA256 != hex.EncodeToString(sum[:]) || a.SizeBytes != int64(len(payload)) || a.MinAgentVersion != "1.2.16" ||
		a.ObjectKey != "agent_workdir/_codex/codex-setup-0.42.0.exe" || !strings.HasPrefix(a.Source, "oss:") {
		t.Fatalf("artifact = %+v", a)
	}
	if len(fs.objects) != before {
		t.Fatalf("registering must write nothing to the bucket: %d objects before, %d after", before, len(fs.objects))
	}
	if pol, _, _ := s.dbm.ops.CurrentPolicy(t.Context()); pol.CodexVersion != "" {
		t.Fatal("registering must not aim the fleet")
	}
}

func TestVersionIsReadOffTheLibraryKey(t *testing.T) {
	cases := map[[2]string]string{
		{"codex", "agent_workdir/_codex/codex-setup-26.901.51231-b8.exe"}: "26.901.51231-b8",
		{"codex", "agent_workdir/_codex/notes.txt"}:                       "",
		{"agent", "agent_workdir/_agent/1.2.16/agent.exe"}:                "1.2.16",
		{"agent", "agent_workdir/_agent/agent.exe"}:                       "",
	}
	for in, want := range cases {
		if got := versionFromKey(in[0], in[1]); got != want {
			t.Errorf("versionFromKey(%s, %s) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestThePageListsWhatCIUploadedUntilItIsRegistered(t *testing.T) {
	s, fs := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	fs.objects["agent_workdir/_codex/codex-setup-0.42.0.exe"] = bytes.Repeat([]byte("codex"), 1024)
	fs.objects["agent_workdir/_agent/agent.exe"] = []byte("fixed key, not a version")

	page := dbGet(t, h, "/releases", cookie)
	body := page.Body.String()
	if !strings.Contains(body, `name="version" value="0.42.0"`) {
		t.Fatal("the unregistered package is not offered for registration")
	}
	if strings.Contains(body, "fixed key") {
		t.Fatal("the fixed agent key is not a version and must not be listed")
	}
	csrf := csrfFrom(t, s, cookie, "/releases")
	rec := dbPost(t, h, "/releases/register", url.Values{"csrf": {csrf}, "product": {"codex"}, "version": {"0.42.0"}}, cookie)
	if !s.jobs.wait(10e9) {
		t.Fatal("job did not finish")
	}
	if snap := s.jobs.snapshot(); snap == nil || snap.Err != "" {
		t.Fatalf("register: redirect %s, job %+v", rec.Header().Get("Location"), snap)
	}
	page = dbGet(t, h, "/releases", cookie)
	// The unregistered table is gone now; the version shows only in the
	// library table (whose "set as fleet target" form also carries it).
	if strings.Contains(page.Body.String(), "未能自动登记的包") {
		t.Fatalf("a registered package must leave the unregistered list (redirect %s)", rec.Header().Get("Location"))
	}
	if !strings.Contains(page.Body.String(), `action="/releases/global"`) {
		t.Fatal("the registered version should now be in the library with its actions")
	}
}

func TestUnregisteredReleaseDeleteRemovesOnlyVersionedObject(t *testing.T) {
	s, fs := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	key := "agent_workdir/_agent/1.4.0/agent.exe"
	fs.objects[key] = []byte("old agent")
	csrf := csrfFrom(t, s, cookie, "/releases")
	rec := dbPost(t, h, "/releases/delete-unregistered", url.Values{
		"csrf": {csrf}, "product": {repo.ProductAgent}, "version": {"1.4.0"}, "confirm": {"1.4.0"},
	}, cookie)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("delete unregistered: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := fs.objects[key]; ok {
		t.Fatal("unregistered OSS package was not deleted")
	}
}

func TestTheScanButtonQueuesAScan(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	page := dbGet(t, h, "/releases", cookie)
	if !strings.Contains(page.Body.String(), `action="/releases/scan"`) {
		t.Fatal("no scan button")
	}
	csrf := csrfFrom(t, s, cookie, "/releases")
	rec := dbPost(t, h, "/releases/scan", url.Values{"csrf": {csrf}}, cookie)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("scan: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	tasks, _ := s.dbm.store.Tasks().ListOpen(t.Context(), 10)
	found := false
	for _, task := range tasks {
		found = found || task.Kind == "release_scan"
	}
	if !found {
		t.Fatal("no release_scan task was queued")
	}
}

func TestAVersionsRemarkCanBeEdited(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	a, _ := s.dbm.ops.RegisterArtifact(t.Context(), repo.NewArtifact{Product: repo.ProductAgent, Version: "1.3.0",
		SHA256: strings.Repeat("e", 64), SizeBytes: 1, ObjectKey: "k", CreatedBy: "t"}, "t", "r")
	csrf := csrfFrom(t, s, cookie, "/releases")
	rec := dbPost(t, h, "/releases/notes", url.Values{"csrf": {csrf}, "id": {a.ID}, "notes": {"  直连控制台  "}}, cookie)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("save notes: %s", loc)
	}
	got, _ := s.dbm.store.Releases().ArtifactByID(t.Context(), a.ID)
	if got.Notes != "直连控制台" {
		t.Fatalf("notes = %q", got.Notes)
	}
	if !strings.Contains(dbGet(t, h, "/releases", cookie).Body.String(), `>直连控制台</textarea>`) {
		t.Fatal("the page should show the remark in its edit box")
	}
}

func TestRetiredReleaseDeleteRemovesOSSBytesAndKeepsHistory(t *testing.T) {
	s, fs := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	a, err := s.dbm.ops.RegisterArtifact(t.Context(), repo.NewArtifact{
		Product: repo.ProductAgent, Version: "1.3.0", SHA256: strings.Repeat("f", 64), SizeBytes: 1,
		ObjectKey: "agent_workdir/_agent/1.3.0/agent.exe", CreatedBy: "t",
	}, "t", "r")
	if err != nil {
		t.Fatal(err)
	}
	fs.objects[a.ObjectKey] = []byte("agent")
	if _, err := s.dbm.ops.SetArtifactStatus(t.Context(), a.ID, repo.ArtifactRetired, "old", "admin", "r2"); err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, s, cookie, "/releases")
	rec := dbPost(t, h, "/releases/delete", url.Values{
		"csrf": {csrf}, "id": {a.ID}, "confirm": {a.Version},
	}, cookie)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("delete: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := fs.objects[a.ObjectKey]; ok {
		t.Fatal("retired OSS package was not deleted")
	}
	got, err := s.dbm.store.Releases().ArtifactByID(t.Context(), a.ID)
	if err != nil || got.Status != repo.ArtifactRetired {
		t.Fatalf("artifact history = %+v, err=%v", got, err)
	}
}
