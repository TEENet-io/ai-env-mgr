package deviceapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func newStore(t *testing.T) (*dbstore.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN is not set; skipping the PostgreSQL integration tests")
	}
	ctx := context.Background()
	dsn, err := dbstore.TestDatabaseDSN(ctx, dsn, "aienv_test_deviceapi")
	if err != nil {
		t.Fatalf("test database: %v", err)
	}
	database, err := dbstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(database.Close)
	if _, err := database.Pool().Exec(ctx, `drop schema public cascade; create schema public; grant all on schema public to public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return dbstore.NewStore(database), ctx
}

// fakeSigner records what it was asked to sign.
type fakeSigner struct{ signed []string }

func (f *fakeSigner) SignedURL(key string, ttl time.Duration) (string, error) {
	f.signed = append(f.signed, "GET "+key)
	return "https://bucket.example/" + key + "?sig=get", nil
}
func (f *fakeSigner) SignedPutURL(key string, ttl time.Duration, ct string) (string, error) {
	f.signed = append(f.signed, "PUT "+key+" "+ct)
	return "https://bucket.example/" + key + "?sig=put", nil
}

type fakeBucketReader map[string][]byte

func (f fakeBucketReader) Get(key string) ([]byte, string, error) {
	b, ok := f[key]
	if !ok {
		return nil, "", repo.ErrNotFound
	}
	return b, "etag", nil
}

// fakeEvents keeps every line so a test can grep it for secrets.
type fakeEvents struct {
	mu    sync.Mutex
	lines []string
}

func (f *fakeEvents) Ops(level, eventType, msg string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, fmt.Sprintf("%s %s %s %v", level, eventType, msg, fields))
}

var lastEvents *fakeEvents

func newServer(t *testing.T) (*Server, *dbstore.Store, context.Context, http.Handler, *fakeSigner, *fakeEvents) {
	t.Helper()
	store, ctx := newStore(t)
	signer := &fakeSigner{}
	events := &fakeEvents{}
	lastEvents = events
	s := &Server{Store: store, Objects: signer, Hub: NewHub(), Events: events, WaitMax: 300 * time.Millisecond, Recheck: 100 * time.Millisecond}
	return s, store, ctx, s.Handler(), signer, events
}

func do(h http.Handler, method, path, token string, body any, headers ...string) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		rd = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "203.0.113.9:4321"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func enrol(t *testing.T, h http.Handler, hostname string) (string, string) {
	t.Helper()
	rec := do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: hostname, AgentVersion: "1.3.0"})
	if rec.Code != 200 {
		var lines []string
		if lastEvents != nil {
			lines = lastEvents.lines
		}
		t.Fatalf("enrol %s: %d %s\nevents: %v", hostname, rec.Code, rec.Body.String(), lines)
	}
	var out enrolResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.DeviceID, out.DeviceToken
}

func publishPolicy(t *testing.T, ctx context.Context, store *dbstore.Store, p model.Policy) {
	t.Helper()
	data, _ := json.Marshal(p)
	if _, err := store.Policies().Publish(ctx, data, "test", "admin"); err != nil {
		t.Fatal(err)
	}
}

func TestEnrolThenConfigThenStatus(t *testing.T) {
	_, store, ctx, h, _, _ := newServer(t)
	publishPolicy(t, ctx, store, model.Policy{BlockEnabled: true, BlockedDomains: []string{"openai.com"}, SyncIntervalMinutes: 60})

	if rec := do(h, "GET", "/agent/v1/config", "", nil); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := do(h, "GET", "/agent/v1/config", "nonsense", nil); rec.Code != 401 {
		t.Fatalf("bad token: %d", rec.Code)
	}
	deviceID, token := enrol(t, h, "PC-1")
	device, err := store.Devices().ByID(ctx, deviceID)
	if err != nil || device.Channel != repo.ChannelAPI || device.EnrolledFrom != "203.0.113.9" || device.AgentVersion != "1.3.0" {
		t.Fatalf("enrolled device = %+v %v", device, err)
	}

	rec := do(h, "GET", "/agent/v1/config", token, nil, "X-Agent-Version", "1.3.0")
	if rec.Code != 200 {
		t.Fatalf("config: %d %s", rec.Code, rec.Body.String())
	}
	var cfg struct {
		ETag       string       `json:"etag"`
		Policy     model.Policy `json:"policy"`
		HasBinding bool         `json:"hasBinding"`
	}
	json.Unmarshal(rec.Body.Bytes(), &cfg)
	if cfg.ETag == "" || rec.Header().Get("ETag") != cfg.ETag || !cfg.Policy.BlockEnabled || cfg.HasBinding {
		t.Fatalf("config = %+v", cfg)
	}
	if rec := do(h, "GET", "/agent/v1/config", token, nil, "If-None-Match", cfg.ETag); rec.Code != 304 {
		t.Fatalf("same etag: %d", rec.Code)
	}

	// The status report goes straight into the table the console reads.
	report := model.Status{Machine: "pc-1", AgentVersion: "1.3.0", LastSync: time.Now().UTC().Format(time.RFC3339), PolicyETag: cfg.ETag, AppLockerMode: "Enforce"}
	if rec := do(h, "POST", "/agent/v1/status", token, report); rec.Code != 204 {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	got, err := store.Reports().Get(ctx, deviceID)
	if err != nil || got.AgentVersion != "1.3.0" || got.AppLockerMode != "Enforce" {
		t.Fatalf("report = %+v %v", got, err)
	}
	if device, _ = store.Devices().ByID(ctx, deviceID); device.LastSeenAt == nil {
		t.Fatal("status must mark the machine seen")
	}
	if rec := do(h, "POST", "/agent/v1/status", token, model.Status{Machine: "PC-2"}); rec.Code != 400 {
		t.Fatalf("another machine's report: %d", rec.Code)
	}
	if rec := do(h, "POST", "/agent/v1/log", token, []byte("line 1\nline 2\n")); rec.Code != 204 {
		t.Fatalf("log: %d", rec.Code)
	}
	if device, _ = store.Devices().ByID(ctx, deviceID); device.LogTail != "line 1\nline 2\n" {
		t.Fatalf("log tail = %q", device.LogTail)
	}
	events, _, _ := store.Audit().Search(ctx, repo.AuditFilter{Action: "device.enrol", Limit: 5})
	if len(events) != 1 || events[0].TargetID != deviceID || events[0].ActorID != "device:PC-1" {
		t.Fatalf("audit = %+v", events)
	}
}

func TestLeasedApplicationEndpointsAuthorizeDatabaseTask(t *testing.T) {
	s, store, ctx, h, signer, _ := newServer(t)
	deviceID, token := enrol(t, h, "PC-TASK")
	task, err := store.ApplicationTasks().Create(ctx, deviceID, "editor", "1.0", "admin", false)
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.ApplicationTasks().Claim(ctx, deviceID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID == "" || task.LeaseToken == "" {
		t.Fatalf("claimed task = %+v", task)
	}
	installer := []byte("installer")
	sum := sha256.Sum256(installer)
	manifest, _ := json.Marshal(model.Application{
		AppID: "editor", Version: "1.0", InstallerType: "msi", Enabled: true, Approved: true,
		ObjectKey: ossclient.ApplicationPackageKey("editor", "1.0", "msi"), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(installer)),
	})
	s.Bucket = fakeBucketReader{ossclient.ApplicationKey("editor", "1.0"): manifest}

	rec := do(h, "GET", "/agent/v1/application-tasks/"+task.ID+"/manifest", token, nil, applicationLeaseHeader, task.LeaseToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("task manifest: %d %s", rec.Code, rec.Body.String())
	}
	var got model.Application
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.AppID != "editor" {
		t.Fatalf("manifest = %+v: %v", got, err)
	}
	rec = do(h, "GET", "/agent/v1/application-tasks/"+task.ID+"/download", token, nil, applicationLeaseHeader, task.LeaseToken)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "editor") {
		t.Fatalf("task download: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(signer.signed) == 0 || signer.signed[len(signer.signed)-1] != "GET "+ossclient.ApplicationPackageKey("editor", "1.0", "msi") {
		t.Fatalf("signed objects = %v", signer.signed)
	}
	if rec := do(h, "GET", "/agent/v1/application-tasks/"+task.ID+"/manifest", token, nil, applicationLeaseHeader, "wrong"); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong lease: %d %s", rec.Code, rec.Body.String())
	}
}

func TestASecondEnrolIsRefusedUntilAllowed(t *testing.T) {
	s, store, ctx, h, _, _ := newServer(t)
	deviceID, first := enrol(t, h, "PC-1")
	rec := do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: "pc-1"})
	if rec.Code != 409 {
		t.Fatalf("second enrol: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/agent/v1/config", first, nil); rec.Code != 200 {
		t.Fatal("the refused attempt must not disturb the real machine's token")
	}
	if err := store.Devices().AllowReenrol(ctx, deviceID, s.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, second := enrol(t, h, "PC-1")
	if rec := do(h, "GET", "/agent/v1/config", first, nil); rec.Code != 401 {
		t.Fatal("re-enrolment must retire the old token")
	}
	if rec := do(h, "GET", "/agent/v1/config", second, nil); rec.Code != 200 {
		t.Fatal("the new token works")
	}
	// The window closed with the enrolment.
	if rec := do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: "PC-1"}); rec.Code != 409 {
		t.Fatalf("third enrol: %d", rec.Code)
	}
	// Rotation: the old token keeps working for the grace period.
	rec = do(h, "POST", "/agent/v1/token/rotate", second, nil)
	if rec.Code != 200 {
		t.Fatalf("rotate: %d", rec.Code)
	}
	var rotated map[string]string
	json.Unmarshal(rec.Body.Bytes(), &rotated)
	if rec := do(h, "GET", "/agent/v1/config", second, nil); rec.Code != 200 {
		t.Fatal("the rotated-out token still works inside the grace")
	}
	if rec := do(h, "GET", "/agent/v1/config", rotated["deviceToken"], nil); rec.Code != 200 {
		t.Fatal("the rotated-in token works")
	}
	// A forgotten machine is locked out: its token stops working and it
	// cannot enrol again until an administrator lets it.
	store.Devices().Revoke(ctx, deviceID)
	if rec := do(h, "GET", "/agent/v1/config", rotated["deviceToken"], nil); rec.Code != 401 {
		t.Fatal("a forgotten machine's token must stop working")
	}
	if rec := do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: "PC-1"}); rec.Code != 409 {
		t.Fatalf("a forgotten machine enrolling: %d", rec.Code)
	}
	store.Devices().AllowReenrol(ctx, deviceID, s.now().Add(time.Hour))
	back, again := enrol(t, h, "PC-1")
	if d, _ := store.Devices().ByID(ctx, back); back != deviceID || d.Status == repo.DeviceRevoked {
		t.Fatalf("let back in, it comes back as itself: %+v", d)
	}
	if rec := do(h, "GET", "/agent/v1/config", again, nil); rec.Code != 200 {
		t.Fatal("the returned machine's token works")
	}
}

func TestAStrangerGetsPolicyButNoSecrets(t *testing.T) {
	_, store, ctx, h, signer, _ := newServer(t)
	publishPolicy(t, ctx, store, model.Policy{BlockEnabled: true, CodexVersion: "0.42.0"})
	_, token := enrol(t, h, "STRANGER")
	if rec := do(h, "GET", "/agent/v1/config", token, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"blockEnabled":true`) {
		t.Fatalf("policy: %d", rec.Code)
	}
	if rec := do(h, "GET", "/agent/v1/credentials", token, nil); rec.Code != 404 {
		t.Fatalf("credentials for an unbound machine: %d", rec.Code)
	}
	if rec := do(h, "POST", "/agent/v1/collect/upload-url", token, uploadRequest{User: "work1", Rel: "2026/09/a.jsonl"}); rec.Code != 403 {
		t.Fatalf("upload for an unbound machine: %d", rec.Code)
	}
	if len(signer.signed) != 0 {
		t.Fatalf("nothing may be signed for a stranger: %v", signer.signed)
	}
}

func TestEnrolHonoursTheCIDRList(t *testing.T) {
	s, _, _, _, _, _ := newServer(t)
	_, allowed, _ := net.ParseCIDR("198.51.100.0/24")
	s.EnrolCIDRs = []net.IPNet{*allowed}
	h := s.Handler()
	if rec := do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: "PC-1"}); rec.Code != 403 {
		t.Fatalf("from outside the list: %d", rec.Code)
	}
	req := httptest.NewRequest("POST", "/agent/v1/enrol", bytes.NewReader([]byte(`{"hostname":"PC-1"}`)))
	req.RemoteAddr = "198.51.100.7:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("from inside the list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEnrolIsRateLimited(t *testing.T) {
	_, _, _, h, _, _ := newServer(t)
	codes := map[int]int{}
	for i := 0; i < enrolPerMin+3; i++ {
		rec := do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: fmt.Sprintf("PC-%d", i)})
		codes[rec.Code]++
	}
	if codes[200] != enrolPerMin || codes[429] != 3 {
		t.Fatalf("codes = %v", codes)
	}
}

func TestWaitReturnsWhenWoken(t *testing.T) {
	s, store, ctx, h, _, _ := newServer(t)
	publishPolicy(t, ctx, store, model.Policy{BlockEnabled: false})
	deviceID, token := enrol(t, h, "PC-1")
	rec := do(h, "GET", "/agent/v1/config", token, nil)
	etag := rec.Header().Get("ETag")

	// Nothing changes: the poll ends with 204 after WaitMax.
	start := time.Now()
	if rec := do(h, "GET", "/agent/v1/wait?etag="+etag, token, nil); rec.Code != 204 {
		t.Fatalf("quiet wait: %d", rec.Code)
	}
	if time.Since(start) < s.WaitMax {
		t.Fatal("the quiet wait returned early")
	}

	// A change plus a wake: the poll returns at once with the new config.
	s.WaitMax = 5 * time.Second
	s.Recheck = 5 * time.Second
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do(h, "GET", "/agent/v1/wait?etag="+etag, token, nil) }()
	time.Sleep(50 * time.Millisecond)
	store.Devices().RequestSync(ctx, deviceID, "n1")
	s.Hub.Wake(deviceID)
	select {
	case rec := <-done:
		if rec.Code != 200 || rec.Header().Get("ETag") == etag || !strings.Contains(rec.Body.String(), `"syncRequested":"n1"`) {
			t.Fatalf("woken wait: %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the wake did not end the poll")
	}

	// Without a wake, the recheck still notices within Recheck.
	s.Recheck = 100 * time.Millisecond
	rec = do(h, "GET", "/agent/v1/config", token, nil)
	etag = rec.Header().Get("ETag")
	go func() { done <- do(h, "GET", "/agent/v1/wait?etag="+etag, token, nil) }()
	time.Sleep(50 * time.Millisecond)
	publishPolicy(t, ctx, store, model.Policy{BlockEnabled: true})
	select {
	case rec := <-done:
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"blockEnabled":true`) {
			t.Fatalf("rechecked wait: %d", rec.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the recheck did not notice the policy change")
	}
}

func TestArtifactAndUploadLinksAreScoped(t *testing.T) {
	_, store, ctx, h, signer, _ := newServer(t)
	publishPolicy(t, ctx, store, model.Policy{CodexVersion: "0.42.0"})
	for _, v := range []string{"0.42.0", "0.43.0"} {
		if _, err := store.Releases().CreateArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: v, SHA256: strings.Repeat("a", 64), SizeBytes: 1, ObjectKey: "agent_workdir/_codex/codex-setup-" + v + ".exe", CreatedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	deviceID, token := enrol(t, h, "PC-1")
	rec := do(h, "GET", "/agent/v1/artifact/codex/0.42.0", token, nil)
	if rec.Code != 302 || !strings.Contains(rec.Header().Get("Location"), "codex-setup-0.42.0.exe") || rec.Header().Get("X-Artifact-SHA256") != strings.Repeat("a", 64) {
		t.Fatalf("fleet target: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(h, "GET", "/agent/v1/artifact/codex/0.43.0", token, nil); rec.Code != 403 {
		t.Fatalf("a version the machine is not aimed at: %d", rec.Code)
	}
	if rec := do(h, "GET", "/agent/v1/artifact/agent/0.42.0", token, nil); rec.Code != 403 {
		t.Fatalf("no agent target: %d", rec.Code)
	}

	// Bound to work1: uploads for work1 only, under that user's directory.
	e, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	store.Bindings().Bind(ctx, deviceID, e.ID, "", "admin")
	rec = do(h, "POST", "/agent/v1/collect/upload-url", token, uploadRequest{User: "Work1", Rel: "2026/09/a.jsonl"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "sig=put") {
		t.Fatalf("upload url: %d %s", rec.Code, rec.Body.String())
	}
	if want := "PUT " + ossclient.DataCollectKey("work1", "2026/09/a.jsonl") + " application/octet-stream"; signer.signed[len(signer.signed)-1] != want {
		t.Fatalf("signed %q", signer.signed[len(signer.signed)-1])
	}
	if rec := do(h, "POST", "/agent/v1/collect/upload-url", token, uploadRequest{User: "work2", Rel: "x"}); rec.Code != 403 {
		t.Fatalf("another user: %d", rec.Code)
	}
	if rec := do(h, "POST", "/agent/v1/collect/upload-url", token, uploadRequest{User: "work1", Rel: "../../policy.json"}); rec.Code != 400 {
		t.Fatalf("path escape: %d", rec.Code)
	}
	// Credentials: 404 until a bundle exists, then the bytes with an etag.
	if rec := do(h, "GET", "/agent/v1/credentials", token, nil); rec.Code != 404 {
		t.Fatalf("no bundle yet: %d", rec.Code)
	}
	store.CredentialBundles().Put(ctx, e.ID, e.AuthEpoch, []byte("PK-zip"), "etag-1")
	rec = do(h, "GET", "/agent/v1/credentials", token, nil)
	if rec.Code != 200 || rec.Body.String() != "PK-zip" || rec.Header().Get("ETag") != "etag-1" {
		t.Fatalf("credentials: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/agent/v1/credentials", token, nil, "If-None-Match", "etag-1"); rec.Code != 304 {
		t.Fatalf("unchanged credentials: %d", rec.Code)
	}
}

func TestTokensNeverAppearInLogs(t *testing.T) {
	_, _, _, h, _, events := newServer(t)
	_, token := enrol(t, h, "PC-1")
	do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: "PC-1"})
	rec := do(h, "POST", "/agent/v1/token/rotate", token, nil)
	var rotated map[string]string
	json.Unmarshal(rec.Body.Bytes(), &rotated)
	for _, line := range events.lines {
		if strings.Contains(line, token) || strings.Contains(line, rotated["deviceToken"]) {
			t.Fatalf("a token leaked into the log: %s", line)
		}
	}
	if len(events.lines) < 2 {
		t.Fatalf("expected enrolment and refusal to be logged: %v", events.lines)
	}
}

// The fleet that has never enrolled is the case that matters: every one of
// its machines is known, assigned, and holds no token. A stranger naming
// one must not get its assignee's credentials.
func TestAKnownMachineIsNotHandedToWhoeverNamesIt(t *testing.T) {
	s, store, ctx, h, _, _ := newServer(t)
	publishPolicy(t, ctx, store, model.Policy{BlockEnabled: true})
	e, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	bound, _ := store.Devices().EnsureByHostname(ctx, "PC-BOUND")
	store.Bindings().Bind(ctx, bound.ID, e.ID, "", "admin")
	store.CredentialBundles().Put(ctx, e.ID, e.AuthEpoch, []byte("PK-secret"), "c1")
	wasBound, _ := store.Devices().EnsureByHostname(ctx, "PC-WAS-BOUND")
	store.Bindings().Bind(ctx, wasBound.ID, e.ID, "", "admin")
	store.Bindings().Unbind(ctx, wasBound.ID, "admin")
	spare, _ := store.Devices().EnsureByHostname(ctx, "PC-SPARE") // seen, never assigned

	for _, name := range []string{"PC-BOUND", "pc-bound", "PC-WAS-BOUND"} {
		if rec := do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: name}); rec.Code != 409 {
			t.Fatalf("%s: %d, want 409", name, rec.Code)
		}
	}
	if _, token := enrol(t, h, "PC-SPARE"); token == "" {
		t.Fatal("a machine nobody was ever assigned to may enrol on its own")
	}
	if d, _ := store.Devices().ByID(ctx, spare.ID); d.Channel != repo.ChannelAPI {
		t.Fatal("the spare is on the api channel now")
	}
	// The administrator opens the door for the bound one; its assignee's
	// bundle is then served to the machine that walked through it.
	store.Devices().AllowReenrol(ctx, bound.ID, s.now().Add(time.Hour))
	_, token := enrol(t, h, "PC-BOUND")
	if rec := do(h, "GET", "/agent/v1/credentials", token, nil); rec.Code != 200 || rec.Body.String() != "PK-secret" {
		t.Fatalf("credentials after the door was opened: %d", rec.Code)
	}
	// Once through, the door is shut again.
	if rec := do(h, "POST", "/agent/v1/enrol", "", enrolRequest{Hostname: "PC-BOUND"}); rec.Code != 409 {
		t.Fatalf("second walk-through: %d", rec.Code)
	}
}

type fakeBucket struct{ objects map[string][]byte }

func (f fakeBucket) Get(key string) ([]byte, string, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, "", ossclient.ErrNotFound
	}
	return data, "oss-etag", nil
}

func TestCredentialsFallBackToTheBucketUntilABundleExists(t *testing.T) {
	s, store, ctx, h, _, _ := newServer(t)
	publishPolicy(t, ctx, store, model.Policy{})
	e, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	deviceID, token := enrol(t, h, "PC-1")
	store.Bindings().Bind(ctx, deviceID, e.ID, "", "admin")
	bucket := fakeBucket{objects: map[string][]byte{ossclient.UserKey("work1", "credentials.zip"): []byte("PK-from-bucket")}}
	s.Bucket = bucket

	rec := do(h, "GET", "/agent/v1/credentials", token, nil)
	if rec.Code != 200 || rec.Body.String() != "PK-from-bucket" || rec.Header().Get("ETag") != "oss-etag" {
		t.Fatalf("bucket fallback: %d %q", rec.Code, rec.Body.String())
	}
	// A bundle in the table wins once it exists.
	store.CredentialBundles().Put(ctx, e.ID, e.AuthEpoch, []byte("PK-from-table"), "t1")
	if rec := do(h, "GET", "/agent/v1/credentials", token, nil); rec.Body.String() != "PK-from-table" {
		t.Fatalf("table bundle: %q", rec.Body.String())
	}
	// With the bucket switched off there is no fallback: 404, as documented.
	store.CredentialBundles().Purge(ctx, e.ID)
	store.Settings().Set(ctx, repo.SettingDeviceChannel, []byte(`{"write_oss_objects":false,"import_oss_status":false}`), 0, "admin")
	if rec := do(h, "GET", "/agent/v1/credentials", token, nil); rec.Code != 404 {
		t.Fatalf("bucket off: %d", rec.Code)
	}
}

func TestArtifactFallsBackToThePolicyKeyForOldVersions(t *testing.T) {
	_, store, ctx, h, signer, _ := newServer(t)
	publishPolicy(t, ctx, store, model.Policy{AgentUpdateVersion: "1.2.14", AgentUpdateSHA256: strings.Repeat("b", 64),
		CodexVersion: "26.901", CodexKey: "agent_workdir/_codex/codex-setup-26.901.exe", CodexSHA256: strings.Repeat("c", 64)})
	_, token := enrol(t, h, "PC-1")
	rec := do(h, "GET", "/agent/v1/artifact/agent/1.2.14", token, nil)
	if rec.Code != 302 || !strings.Contains(rec.Header().Get("Location"), ossclient.AgentBinaryKey()) || rec.Header().Get("X-Artifact-SHA256") != strings.Repeat("b", 64) {
		t.Fatalf("agent fallback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(h, "GET", "/agent/v1/artifact/codex/26.901", token, nil)
	if rec.Code != 302 || !strings.Contains(rec.Header().Get("Location"), "codex-setup-26.901.exe") {
		t.Fatalf("codex fallback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(signer.signed) != 2 {
		t.Fatalf("signed %v", signer.signed)
	}
}
