package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// fakeUpdater records the binaries it was asked to apply.
type fakeUpdater struct {
	applied [][]byte
	err     error
}

func (u *fakeUpdater) ApplyUpdate(b []byte) error {
	u.applied = append(u.applied, b)
	return u.err
}

// setupUpdate seeds the store with an agent binary and a policy targeting the
// given version, and returns the syncer wired with a fake updater.
func setupUpdate(t *testing.T, targetVersion, sha string, bin []byte) (*Syncer, *fakeStore, *fakeUpdater) {
	t.Helper()
	store := newFakeStore()
	s := newSyncer(t, store, &fakeApplier{}) // Version == "test"
	up := &fakeUpdater{}
	s.Updater = up
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.AgentBinaryKey(), bin, "binetag")
	pol := model.DefaultPolicy()
	pol.AgentUpdateVersion = targetVersion
	pol.AgentUpdateSHA256 = sha
	store.set(ossclient.PolicyKey(), policyBytes(t, pol), "petag")
	return s, store, up
}

func TestRunOnceAppliesUpdateWhenTargetDiffers(t *testing.T) {
	bin := []byte("new agent binary v1.2.0")
	sum := sha256.Sum256(bin)
	s, _, up := setupUpdate(t, "1.2.0", hex.EncodeToString(sum[:]), bin)

	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(up.applied) != 1 || string(up.applied[0]) != string(bin) {
		t.Fatalf("update not applied as expected: %d call(s)", len(up.applied))
	}
}

func TestRunOnceSkipsUpdateWhenSameVersion(t *testing.T) {
	bin := []byte("binary")
	sum := sha256.Sum256(bin)
	// target == the syncer's own version ("test"): nothing to do.
	s, _, up := setupUpdate(t, "test", hex.EncodeToString(sum[:]), bin)
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(up.applied) != 0 {
		t.Fatalf("should not update when already on target; got %d call(s)", len(up.applied))
	}
}

func TestRunOnceSkipsUpdateOnChecksumMismatch(t *testing.T) {
	bin := []byte("real binary")
	s, _, up := setupUpdate(t, "1.2.0", "not-the-real-sha", bin)
	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(up.applied) != 0 {
		t.Fatal("must not apply a binary whose checksum does not match")
	}
	found := false
	for _, e := range st.Errors {
		if strings.Contains(e, "checksum mismatch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a checksum-mismatch error in status, got %v", st.Errors)
	}
}

func TestRunOnceSkipsUpdateAlreadyAttempted(t *testing.T) {
	bin := []byte("binary v1.2.0")
	sum := sha256.Sum256(bin)
	s, _, up := setupUpdate(t, "1.2.0", hex.EncodeToString(sum[:]), bin)
	// Pretend this exact target was already tried: it must not loop.
	if err := os.WriteFile(filepath.Join(s.StateDir, updateMarkerFile), []byte("1.2.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(up.applied) != 0 {
		t.Fatalf("must not re-attempt an already-tried target; got %d call(s)", len(up.applied))
	}
}

func TestHeartbeatBeforeSyncWritesNothing(t *testing.T) {
	store := newFakeStore()
	s := newSyncer(t, store, &fakeApplier{})
	if err := s.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	if len(store.puts) != 0 {
		t.Fatalf("heartbeat before the first sync should write nothing, wrote %d", len(store.puts))
	}
}

func TestHeartbeatRefreshesStatusAfterSync(t *testing.T) {
	store := newFakeStore()
	s := newSyncer(t, store, &fakeApplier{})
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.DefaultPolicy()), "p")
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if err := s.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	out := store.puts[ossclient.StatusKey("DESKTOP-A")]
	if out == nil {
		t.Fatal("heartbeat did not write the status object")
	}
	var st model.Status
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatal(err)
	}
	if st.Machine != "DESKTOP-A" || st.LastSync == "" {
		t.Fatalf("heartbeat status = %+v; want machine set and a fresh LastSync", st)
	}
}

// The fake must model the real store's contract: a missing object reports
// ossclient.ErrNotFound, which is what tells the agent a revocation happened
// rather than a network failure.
var errNotFound = fmt.Errorf("fake store: %w", ossclient.ErrNotFound)

// fakeStore is an in-memory Store.
type fakeStore struct {
	objects map[string][]byte
	etags   map[string]string
	puts    map[string][]byte
	putErr  error
	getErrs map[string]error
	gets    []string
}

// downloadCount reports how many times a key was fully downloaded, so a test
// can prove an unchanged object was not fetched again.
func (f *fakeStore) downloadCount(key string) int {
	n := 0
	for _, k := range f.gets {
		if k == key {
			n++
		}
	}
	return n
}

// failKey makes Get return err for one key, standing in for a store that
// could not be reached rather than one that answered "gone".
func (f *fakeStore) failKey(key string, err error) {
	if f.getErrs == nil {
		f.getErrs = map[string]error{}
	}
	f.getErrs[key] = err
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		objects: map[string][]byte{},
		etags:   map[string]string{},
		puts:    map[string][]byte{},
	}
}

func (f *fakeStore) set(key string, data []byte, etag string) {
	f.objects[key] = data
	f.etags[key] = etag
}

// gets counts full downloads, so a test can prove an unchanged object was
// not fetched again.
func (f *fakeStore) Head(key string) (string, bool, error) {
	if err, ok := f.getErrs[key]; ok {
		return "", false, err
	}
	if _, ok := f.objects[key]; !ok {
		return "", false, nil
	}
	return f.etags[key], true, nil
}

func (f *fakeStore) Get(key string) ([]byte, string, error) {
	f.gets = append(f.gets, key)
	if err, ok := f.getErrs[key]; ok {
		return nil, "", err
	}
	d, ok := f.objects[key]
	if !ok {
		return nil, "", errNotFound
	}
	return d, f.etags[key], nil
}

func (f *fakeStore) Put(key string, data []byte) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.puts[key] = data
	return nil
}

// fakeApplier records what would have been applied locally.
type fakeApplier struct {
	applied   []model.Policy
	deployed  []model.CredentialSet
	policyErr error
	deployErr error
	deployedN int
	// deployNone makes DeployCreds report that it wrote nothing, which is
	// what happens when an archive contains no recognised file names.
	deployNone bool

	removedFrom []string
	removeN     int
	removeErr   error
}

func (a *fakeApplier) RemoveCreds(profileDir string) (int, error) {
	a.removedFrom = append(a.removedFrom, profileDir)
	if a.removeErr != nil {
		return 0, a.removeErr
	}
	return a.removeN, nil
}

func (a *fakeApplier) ApplyPolicy(p model.Policy) error {
	if a.policyErr != nil {
		return a.policyErr
	}
	a.applied = append(a.applied, p)
	return nil
}

func (a *fakeApplier) DeployCreds(profileDir string, set model.CredentialSet) (int, error) {
	if a.deployErr != nil {
		return 0, a.deployErr
	}
	a.deployed = append(a.deployed, set)
	if a.deployNone {
		return 0, nil
	}
	if a.deployedN > 0 {
		return a.deployedN, nil
	}
	return len(set), nil
}

// fakeMachine stands in for the real host.
type fakeMachine struct {
	name       string
	localUsers []string
	profileDir string
}

func (m *fakeMachine) Name() string         { return m.name }
func (m *fakeMachine) LocalUsers() []string { return m.localUsers }
func (m *fakeMachine) ProfileDir(user string) string {
	return filepath.Join(m.profileDir, user)
}

func newSyncer(t *testing.T, store *fakeStore, app *fakeApplier) *Syncer {
	t.Helper()
	return &Syncer{
		Store: store, Applier: app,
		Machine: &fakeMachine{
			name:       "DESKTOP-A",
			localUsers: []string{"Administrator", "work1"},
			profileDir: t.TempDir(),
		},
		Version: "test", StateDir: t.TempDir(),
		FallbackInterval: 30,
	}
}

// bind writes the binding that tells the agent which user it serves.
func bind(t *testing.T, store *fakeStore, machine, user string) {
	t.Helper()
	out, err := json.Marshal(model.Binding{User: user, BoundAt: "now"})
	if err != nil {
		t.Fatal(err)
	}
	store.set(ossclient.BindingKey(machine), out, "b1")
}

// fakeCollector stands in for a real Collector so RunOnce's wiring can be
// tested without touching the filesystem.
type fakeCollector struct {
	calls    []string // users it was called for
	uploaded int
}

func (f *fakeCollector) CollectOnce(user string, quietSeconds int, since string) CollectResult {
	f.calls = append(f.calls, user)
	return CollectResult{Uploaded: f.uploaded}
}

func policyBytes(t *testing.T, p model.Policy) []byte {
	t.Helper()
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func credsBytes(t *testing.T) []byte {
	t.Helper()
	blob, err := creds.Pack(model.CredentialSet{model.PathCodexAuth: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func TestRunOnceAppliesPolicyAndCreds(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	pol := model.Policy{BlockEnabled: true, BlockedDomains: []string{"openai.com"}, SyncIntervalMinutes: 15}
	store.set(ossclient.PolicyKey(), policyBytes(t, pol), "p1")
	store.set(ossclient.UserKey("work1", "credentials.zip"), credsBytes(t), "c1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(app.applied) != 1 || !app.applied[0].BlockEnabled {
		t.Errorf("policy not applied: %+v", app.applied)
	}
	if len(app.deployed) != 1 {
		t.Errorf("credentials not deployed: %+v", app.deployed)
	}
	if st.PolicyETag != "p1" || st.CredsETag != "c1" {
		t.Errorf("etags not recorded: %+v", st)
	}
	if !st.CredsApplied {
		t.Error("CredsApplied should be true")
	}
	if st.SyncIntervalMinutes != 15 {
		t.Errorf("SyncIntervalMinutes = %d, want 15 from the policy", st.SyncIntervalMinutes)
	}
	if len(st.Errors) != 0 {
		t.Errorf("unexpected errors: %v", st.Errors)
	}
	if _, ok := store.puts[ossclient.StatusKey("DESKTOP-A")]; !ok {
		t.Error("status was not uploaded")
	}
}

// Re-applying an unchanged policy would rewrite the registry on every cycle
// for no reason, so a matching ETag must short-circuit the work.
func TestRunOnceSkipsWhenETagUnchanged(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "same")
	store.set(ossclient.UserKey("work1", "credentials.zip"), credsBytes(t), "csame")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	if _, err := s.RunOnce(); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if _, err := s.RunOnce(); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if len(app.applied) != 1 {
		t.Errorf("policy applied %d times, want 1", len(app.applied))
	}
	if len(app.deployed) != 1 {
		t.Errorf("creds deployed %d times, want 1", len(app.deployed))
	}
}

func TestRunOnceReappliesWhenETagChanges(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "v1")
	store.set(ossclient.UserKey("work1", "credentials.zip"), credsBytes(t), "c1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}

	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: false}), "v2")
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(app.applied) != 2 {
		t.Errorf("policy applied %d times, want 2 after the etag changed", len(app.applied))
	}
	if app.applied[1].BlockEnabled {
		t.Error("second application should carry the new policy")
	}
}

// A missing credentials object must not stop the policy from being applied.
func TestRunOnceRecordsErrorsWithoutFailing(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce should tolerate a missing credentials object: %v", err)
	}
	if st.CredsApplied {
		t.Error("CredsApplied should be false when no archive exists")
	}
	if len(app.applied) != 1 {
		t.Error("policy should still be applied")
	}
	if len(st.Errors) == 0 {
		t.Error("the missing credentials object should be reported")
	}
}

func TestRunOnceReportsApplyFailure(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")
	store.set(ossclient.UserKey("work1", "credentials.zip"), credsBytes(t), "c1")

	app := &fakeApplier{policyErr: errors.New("registry denied")}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(st.Errors) == 0 {
		t.Fatal("a failing ApplyPolicy should be reported")
	}
	// Credentials must still have been attempted.
	if !st.CredsApplied {
		t.Error("credentials should be delivered even when the policy step failed")
	}
}

// A failed status upload must not discard work already applied locally.
func TestRunOnceSurvivesStatusUploadFailure(t *testing.T) {
	store := newFakeStore()
	store.putErr = errors.New("oss unreachable")
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce should not fail when the status upload fails: %v", err)
	}
	if len(app.applied) != 1 {
		t.Error("policy should still have been applied")
	}
	found := false
	for _, e := range st.Errors {
		if len(e) >= 6 && e[:6] == "status" {
			found = true
		}
	}
	if !found {
		t.Errorf("the upload failure should be reported, got %v", st.Errors)
	}
}

func TestRunOnceUsesFallbackIntervalWhenPolicyOmitsIt(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)
	s.FallbackInterval = 45

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if st.SyncIntervalMinutes != 45 {
		t.Errorf("SyncIntervalMinutes = %d, want the fallback 45", st.SyncIntervalMinutes)
	}
}

func TestRunOnceClampsAbsurdInterval(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	pol := model.Policy{BlockEnabled: true, SyncIntervalMinutes: 99999}
	store.set(ossclient.PolicyKey(), policyBytes(t, pol), "p1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if st.SyncIntervalMinutes != model.MaxSyncInterval {
		t.Errorf("SyncIntervalMinutes = %d, want it clamped to %d", st.SyncIntervalMinutes, model.MaxSyncInterval)
	}
}

func TestRunOncePersistsLastSyncTime(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	if !s.LastSyncTime().IsZero() {
		t.Error("a fresh state directory should report no previous sync")
	}
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	last := s.LastSyncTime()
	if last.IsZero() {
		t.Fatal("last sync time was not persisted")
	}
	if time.Since(last) > time.Minute {
		t.Errorf("last sync time looks wrong: %v", last)
	}
	// The marker must survive a new Syncer instance, i.e. a service restart.
	again := &Syncer{StateDir: s.StateDir}
	if again.LastSyncTime().IsZero() {
		t.Error("last sync time should be readable after a restart")
	}
}

// Cloud desktops sleep; the ticker freezes with them. These cases drive the
// wall-clock check that catches an overdue sync after a wake-up.
func TestDueForSync(t *testing.T) {
	dir := t.TempDir()
	s := &Syncer{StateDir: dir}

	if !s.DueForSync(30 * time.Minute) {
		t.Error("never having synced should count as due")
	}

	s.writeMarker(lastSyncMarkerFile, time.Now().UTC().Format(time.RFC3339))
	if s.DueForSync(30 * time.Minute) {
		t.Error("a sync that just happened should not be due")
	}

	s.writeMarker(lastSyncMarkerFile, time.Now().UTC().Add(-45*time.Minute).Format(time.RFC3339))
	if !s.DueForSync(30 * time.Minute) {
		t.Error("45 minutes without a sync should be due at a 30 minute interval")
	}
}

func TestDueForSyncHandlesClockGoingBackwards(t *testing.T) {
	s := &Syncer{StateDir: t.TempDir()}
	s.writeMarker(lastSyncMarkerFile, time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339))
	if !s.DueForSync(30 * time.Minute) {
		t.Error("a timestamp in the future means the clock moved; treat it as due")
	}
}

func TestDueForSyncIgnoresCorruptMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lastSyncMarkerFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Syncer{StateDir: dir}
	if !s.DueForSync(30 * time.Minute) {
		t.Error("an unreadable marker should fall back to syncing")
	}
}

func TestNextInterval(t *testing.T) {
	s := &Syncer{FallbackInterval: 30}
	if got := s.NextInterval(model.Status{SyncIntervalMinutes: 5}); got != 5*time.Minute {
		t.Errorf("NextInterval = %v, want 5m", got)
	}
	if got := s.NextInterval(model.Status{}); got != 30*time.Minute {
		t.Errorf("NextInterval with no value = %v, want the fallback 30m", got)
	}
	if got := s.NextInterval(model.Status{SyncIntervalMinutes: 99999}); got != time.Duration(model.MaxSyncInterval)*time.Minute {
		t.Errorf("NextInterval = %v, want it clamped", got)
	}
}

// A machine that has just been created has no binding yet. It must still
// report itself so the administrator can find it in `admin status` and
// assign it, rather than staying invisible.
func TestRunOnceWithoutBindingStillReports(t *testing.T) {
	store := newFakeStore()
	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.Machine != "DESKTOP-A" {
		t.Errorf("Machine = %q, want DESKTOP-A", st.Machine)
	}
	if st.BoundUser != "" {
		t.Errorf("BoundUser = %q, want empty", st.BoundUser)
	}
	if len(st.LocalUsers) != 2 {
		t.Errorf("LocalUsers = %v, want the machine's two accounts", st.LocalUsers)
	}
	if len(app.applied) != 0 || len(app.deployed) != 0 {
		t.Error("nothing should be applied without a binding")
	}
	if _, ok := store.puts[ossclient.StatusKey("DESKTOP-A")]; !ok {
		t.Error("an unbound machine must still upload its status")
	}
	if len(st.Errors) == 0 {
		t.Error("the missing binding should be reported")
	}
}

// Binding a machine to an account that does not exist locally is an operator
// mistake. The block still has to be applied because it protects the whole
// machine, but credentials have nowhere to go.
func TestRunOnceWhenBoundUserHasNoProfile(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "ghost")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")
	store.set(ossclient.UserKey("ghost", "credentials.zip"), credsBytes(t), "c1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.BoundUserExists {
		t.Error("BoundUserExists should be false")
	}
	if len(app.applied) != 1 {
		t.Error("the machine-wide block should still be applied")
	}
	if len(app.deployed) != 0 {
		t.Error("credentials must not be delivered when the profile is missing")
	}
	if st.CredsApplied {
		t.Error("CredsApplied should be false")
	}
}

// Windows account names are case-insensitive, so a binding written as "Work1"
// must still match the "work1" profile on disk.
func TestRunOnceMatchesLocalUserIgnoringCase(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "WORK1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")
	store.set(ossclient.UserKey("WORK1", "credentials.zip"), credsBytes(t), "c1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !st.BoundUserExists {
		t.Error("BoundUserExists should be true despite the different case")
	}
	if len(app.deployed) != 1 {
		t.Error("credentials should have been delivered")
	}
}

func TestRunOnceReadsTheBoundUsersDirectory(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	// A decoy under another user's prefix must never be read.
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: false}), "wrong")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "right")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(app.applied) != 1 || !app.applied[0].BlockEnabled {
		t.Errorf("the bound user's policy should have been applied, got %+v", app.applied)
	}
}

func TestRunOnceIgnoresMalformedBinding(t *testing.T) {
	store := newFakeStore()
	store.set(ossclient.BindingKey("DESKTOP-A"), []byte("not json"), "b1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.BoundUser != "" {
		t.Error("a malformed binding should be treated as unbound")
	}
	if len(app.applied) != 0 {
		t.Error("nothing should be applied from a malformed binding")
	}
}

// The whole point of moving the policy out of the employee directory: a
// machine that has just been created from the image, and has not been
// assigned to anybody, must still be locked down. Keying the policy by
// employee left exactly these machines wide open.
func TestRunOnceAppliesPolicyWhenUnbound(t *testing.T) {
	store := newFakeStore()
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{
		BlockEnabled:   true,
		BlockedDomains: []string{"openai.com", "claude.ai"},
	}), "p1")
	// Deliberately no binding.

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(app.applied) != 1 {
		t.Fatalf("policy applied %d times, want 1 on an unbound machine", len(app.applied))
	}
	if !app.applied[0].BlockEnabled {
		t.Error("the block should be in force")
	}
	if !st.BlockEnabled || st.BlockedDomains != 2 {
		t.Errorf("status should report the block: BlockEnabled=%v domains=%d", st.BlockEnabled, st.BlockedDomains)
	}
	if st.CredsApplied {
		t.Error("credentials must not be delivered without a binding")
	}
	if len(app.deployed) != 0 {
		t.Error("nothing to deploy credentials to")
	}
}

// A missing employee profile must not suppress the machine-wide block either.
func TestRunOnceAppliesPolicyWhenBoundUserHasNoProfile(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "ghost")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	if _, err := s.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(app.applied) != 1 {
		t.Errorf("policy applied %d times, want 1", len(app.applied))
	}
}

// An archive that unpacks but contains nothing recognisable used to be
// swallowed: no error, no marker written, retried forever, invisible in
// admin status.
func TestRunOnceReportsAnEmptyCredentialArchive(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")
	store.set(ossclient.UserKey("work1", "credentials.zip"), credsBytes(t), "c1")

	app := &fakeApplier{deployNone: true} // nothing matched the whitelist
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.CredsApplied {
		t.Error("CredsApplied should be false when nothing was written")
	}
	found := false
	for _, e := range st.Errors {
		if strings.Contains(e, "no recognised files") {
			found = true
		}
	}
	if !found {
		t.Errorf("the empty archive should be reported, got errors: %v", st.Errors)
	}
}

// Offboarding reaches the machine through the credentials object going away.
// A definitive 404 means the administrator revoked them, so the local copies
// must go too.
func TestRunOnceRemovesCredentialsWhenTheObjectIsGone(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")
	// No credentials.zip: the store reports it as definitively absent.

	app := &fakeApplier{removeN: 3}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(app.removedFrom) != 1 {
		t.Fatalf("RemoveCreds called %d times, want 1", len(app.removedFrom))
	}
	if st.CredsApplied {
		t.Error("CredsApplied should be false after a revocation")
	}
	found := false
	for _, e := range st.Errors {
		if strings.Contains(e, "revoked") {
			found = true
		}
	}
	if !found {
		t.Errorf("the revocation should be visible in status, got: %v", st.Errors)
	}
}

// The safety property that makes the above acceptable: a store we merely
// could not reach must never be read as a revocation. Otherwise one network
// blip wipes every machine's logins at once.
func TestRunOnceDoesNotRemoveCredentialsOnATransientError(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")
	store.failKey(ossclient.UserKey("work1", "credentials.zip"), errors.New("dial tcp: i/o timeout"))

	app := &fakeApplier{removeN: 3}
	s := newSyncer(t, store, app)

	st, err := s.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(app.removedFrom) != 0 {
		t.Error("a transient store error must not be treated as a revocation")
	}
	if len(st.Errors) == 0 {
		t.Error("the failure should still be reported")
	}
}

// credentials.zip holds the employee's live AI tokens. Once delivered, an
// unchanged archive must not be downloaded again -- at a one-minute sync
// interval that would push those secrets across the network some 1400 times a
// day to learn nothing each time.
func TestRunOnceDoesNotRedownloadUnchangedCredentials(t *testing.T) {
	store := newFakeStore()
	bind(t, store, "DESKTOP-A", "work1")
	store.set(ossclient.PolicyKey(), policyBytes(t, model.Policy{BlockEnabled: true}), "p1")
	credsKey := ossclient.UserKey("work1", "credentials.zip")
	store.set(credsKey, credsBytes(t), "c1")

	app := &fakeApplier{}
	s := newSyncer(t, store, app)

	// First cycle: must download and deliver.
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if got := store.downloadCount(credsKey); got != 1 {
		t.Fatalf("first cycle downloaded credentials %d times, want 1", got)
	}
	if len(app.deployed) != 1 {
		t.Fatalf("first cycle deployed %d times, want 1", len(app.deployed))
	}

	// Several more cycles with nothing changed.
	for i := 0; i < 5; i++ {
		st, err := s.RunOnce()
		if err != nil {
			t.Fatal(err)
		}
		if !st.CredsApplied {
			t.Error("CredsApplied should stay true while the archive is unchanged")
		}
	}
	if got := store.downloadCount(credsKey); got != 1 {
		t.Errorf("credentials downloaded %d times across 6 cycles, want 1", got)
	}
	if len(app.deployed) != 1 {
		t.Errorf("deployed %d times, want 1 -- an unchanged archive must not be redelivered", len(app.deployed))
	}

	// A new archive must be picked up.
	store.set(credsKey, credsBytes(t), "c2")
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if got := store.downloadCount(credsKey); got != 2 {
		t.Errorf("a changed archive should be downloaded, got %d downloads", got)
	}
	if len(app.deployed) != 2 {
		t.Errorf("a changed archive should be redelivered, got %d deploys", len(app.deployed))
	}
}

func TestRunOnceRunsCollectorWhenEnabled(t *testing.T) {
	store := newFakeStore()
	app := &fakeApplier{}
	s := newSyncer(t, store, app)
	// bound to work1, which is a local user in newSyncer's fakeMachine
	bind(t, store, "DESKTOP-A", "work1")
	pol := model.DefaultPolicy()
	pol.CollectEnabled = true
	store.set(ossclient.PolicyKey(), policyBytes(t, pol), "petag")
	col := &fakeCollector{uploaded: 3}
	s.Collector = col

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(col.calls) != 1 || col.calls[0] != "work1" {
		t.Fatalf("collector calls=%v want [work1]", col.calls)
	}
	if !st.CollectEnabled || st.CollectUploaded != 3 {
		t.Fatalf("status collectEnabled=%v uploaded=%d want true/3", st.CollectEnabled, st.CollectUploaded)
	}
}

// The collector must not run on an unbound machine even when collection is
// enabled: there is no employee to attribute the session files to, and
// nothing under Root belongs to "nobody". This locks the `bound` conjunct of
// the guard, separately from the `enabled` conjunct covered above.
func TestRunOnceSkipsCollectorWhenUnbound(t *testing.T) {
	store := newFakeStore()
	app := &fakeApplier{}
	s := newSyncer(t, store, app)
	pol := model.DefaultPolicy()
	pol.CollectEnabled = true
	store.set(ossclient.PolicyKey(), policyBytes(t, pol), "petag")
	// Deliberately no binding.
	col := &fakeCollector{uploaded: 3}
	s.Collector = col

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(col.calls) != 0 {
		t.Fatalf("collector should not run when unbound; calls=%v", col.calls)
	}
	if st.CollectUploaded != 0 {
		t.Fatalf("CollectUploaded = %d, want 0 when unbound", st.CollectUploaded)
	}
}

func TestRunOnceSkipsCollectorWhenDisabled(t *testing.T) {
	store := newFakeStore()
	app := &fakeApplier{}
	s := newSyncer(t, store, app)
	bind(t, store, "DESKTOP-A", "work1")
	// DefaultPolicy has CollectEnabled == false
	store.set(ossclient.PolicyKey(), policyBytes(t, model.DefaultPolicy()), "petag")
	col := &fakeCollector{uploaded: 3}
	s.Collector = col

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(col.calls) != 0 {
		t.Fatalf("collector should not run when disabled; calls=%v", col.calls)
	}
	if st.CollectEnabled || st.CollectUploaded != 0 {
		t.Fatalf("status collectEnabled=%v uploaded=%d want false/0", st.CollectEnabled, st.CollectUploaded)
	}
}
