package admincore

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

var errNotFound = ossclient.ErrNotFound

// fakeStore is an in-memory Store, mirroring agentcore's test double.
type fakeStore struct {
	objects      map[string][]byte
	etags        map[string]string
	mtimes       map[string]time.Time // optional per-key modification time for ListInfo
	putErr       error
	putErrFor    string // when set, Put fails only for this exact key
	getErrFor    string // if non-empty, Get returns this error for this specific key
	getErr       error  // error to return for getErrFor key
	deleteErrFor string // when set, Delete fails only for this exact key
	signed       []signRequest
	signErr      error
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, etags: map[string]string{}, mtimes: map[string]time.Time{}}
}

func (f *fakeStore) Get(key string) ([]byte, string, error) {
	if f.getErrFor != "" && key == f.getErrFor {
		return nil, "", f.getErr
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
	if f.putErrFor != "" && key == f.putErrFor {
		return errNotFound
	}
	f.objects[key] = data
	return nil
}

// List mirrors ossclient.Client.List: every key under the prefix, sorted for
// deterministic test output.
func (f *fakeStore) List(prefix string) ([]string, error) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// ListInfo mirrors ossclient.Client.ListInfo: every object under the prefix
// with its size (from the stored bytes) and its modification time (from the
// optional mtimes map; zero when unset), sorted for deterministic output.
func (f *fakeStore) ListInfo(prefix string) ([]ossclient.ObjectInfo, error) {
	var out []ossclient.ObjectInfo
	for k, d := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, ossclient.ObjectInfo{Key: k, Size: int64(len(d)), LastModified: f.mtimes[k]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Delete removes an object. Deleting a key that is not present is a no-op,
// matching OSS's own delete semantics (idempotent, not an error).
func (f *fakeStore) Delete(key string) error {
	if f.deleteErrFor != "" && key == f.deleteErrFor {
		return errors.New("transient")
	}
	delete(f.objects, key)
	delete(f.etags, key)
	return nil
}

// SignedURL stands in for OSS presigning: the real one produces an opaque
// URL, and all the caller can meaningfully assert is that a link came back
// for the right key with the right lifetime.
func (f *fakeStore) SignedURL(key string, ttl time.Duration) (string, error) {
	if f.signErr != nil {
		return "", f.signErr
	}
	f.signed = append(f.signed, signRequest{Key: key, TTL: ttl})
	return "https://example-bucket.oss/" + key + "?signature=fake", nil
}

type signRequest struct {
	Key string
	TTL time.Duration
}

func newManager() (*Manager, *fakeStore) {
	store := newFakeStore()
	return &Manager{Store: store}, store
}

// addTestUser seeds or updates one roster entry the way the now-removed
// admincore.AddUser used to (find-or-create, mark enabled): Onboard is the
// real entry point since Task 10, but most of the tests here only need a
// roster row to exist and do not care how it got there.
func addTestUser(t *testing.T, m *Manager, windowsUser, codexAccount, claudeAccount string) {
	t.Helper()
	us, err := m.LoadUsers()
	if err != nil {
		t.Fatalf("LoadUsers: %v", err)
	}
	if e := us.Find(windowsUser); e != nil {
		e.CodexAccount, e.ClaudeAccount, e.Enabled = codexAccount, claudeAccount, true
	} else {
		us.Users = append(us.Users, model.UserEntry{
			WindowsUser: windowsUser, CodexAccount: codexAccount, ClaudeAccount: claudeAccount, Enabled: true,
		})
	}
	if err := m.SaveUsers(us); err != nil {
		t.Fatalf("SaveUsers: %v", err)
	}
}

func TestLoadUsersEmptyWhenMissing(t *testing.T) {
	m, _ := newManager()
	us, err := m.LoadUsers()
	if err != nil {
		t.Fatalf("LoadUsers: %v", err)
	}
	if len(us.Users) != 0 {
		t.Errorf("expected empty roster on first run, got %+v", us)
	}
}

func TestSetUserEnabledUnknownUserErrors(t *testing.T) {
	m, _ := newManager()
	if err := m.SetUserEnabled("ghost", false); err == nil {
		t.Fatal("expected error for a user not in the roster")
	}
}

func TestSetUserEnabledPersists(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "work1", "c", "cl")
	if err := m.SetUserEnabled("work1", false); err != nil {
		t.Fatal(err)
	}
	us, err := m.LoadUsers()
	if err != nil {
		t.Fatal(err)
	}
	if us.Find("work1").Enabled {
		t.Error("user should now be disabled")
	}
}

// The block policy is machine-wide, so there is exactly one object and the
// roster has no say in it. A disabled employee, or no employees at all, must
// not stop a machine from being locked down.
func TestPolicyIsASingleObjectIndependentOfTheRoster(t *testing.T) {
	m, store := newManager()
	addTestUser(t, m, "work1", "", "")
	addTestUser(t, m, "work2", "", "")
	if err := m.SetUserEnabled("work2", false); err != nil {
		t.Fatal(err)
	}

	if err := m.PublishPolicy(model.DefaultPolicy()); err != nil {
		t.Fatal(err)
	}

	if _, ok := store.objects[ossclient.PolicyKey()]; !ok {
		t.Fatalf("policy should be written to %s", ossclient.PolicyKey())
	}
	// No per-employee copies, enabled or otherwise.
	for _, user := range []string{"work1", "work2"} {
		if _, ok := store.objects[ossclient.UserKey(user, "policy.json")]; ok {
			t.Errorf("found a per-employee policy copy for %q; there should be exactly one policy object", user)
		}
	}
}

// An empty roster must still produce a policy: machines exist and need
// locking down before anybody is assigned to them.
func TestPolicyPublishesWithNoUsersAtAll(t *testing.T) {
	m, store := newManager()
	if _, err := m.MutateDomains([]string{"gemini.google.com"}, nil); err != nil {
		t.Fatal(err)
	}
	data, ok := store.objects[ossclient.PolicyKey()]
	if !ok {
		t.Fatal("policy should be published even with an empty roster")
	}
	var stored model.Policy
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if !containsString(stored.BlockedDomains, "gemini.google.com") {
		t.Errorf("stored domains = %v, want the added one", stored.BlockedDomains)
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestMutateDomainsAddsRemovesDedupsAndSorts(t *testing.T) {
	m, store := newManager()
	addTestUser(t, m, "work1", "", "")
	if err := m.PublishPolicy(model.Policy{BlockEnabled: true, BlockedDomains: []string{"B.com", "a.com"}}); err != nil {
		t.Fatal(err)
	}

	p, err := m.MutateDomains([]string{"C.com", " a.com ", ""}, []string{"b.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.com", "c.com"}
	if !reflect.DeepEqual(p.BlockedDomains, want) {
		t.Errorf("BlockedDomains = %v, want %v", p.BlockedDomains, want)
	}

	data := store.objects[ossclient.PolicyKey()]
	var stored model.Policy
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.BlockedDomains, want) {
		t.Errorf("stored BlockedDomains = %v, want %v (MutateDomains must broadcast)", stored.BlockedDomains, want)
	}
}

func TestMutateAppLockerAllowPathsValidatesAndNormalises(t *testing.T) {
	m, _ := newManager()
	p, err := m.MutateAppLockerAllowPaths([]string{`c:\TOOLS\codex\*`, `C:\tools\Codex\*`, `D:\Apps\Foo\*`}, nil)
	if err != nil || len(p.AppLockerAllowPaths) != 2 {
		t.Fatalf("got %v %v", p.AppLockerAllowPaths, err)
	}
	if _, err := m.MutateAppLockerAllowPaths([]string{`%LOCALAPPDATA%\x\*`}, nil); err == nil {
		t.Fatal("user-writable path must be refused and nothing published")
	}
	p, _ = m.MutateAppLockerAllowPaths(nil, []string{`C:\TOOLS\CODEX\*`})
	if len(p.AppLockerAllowPaths) != 1 || p.AppLockerAllowPaths[0] != `D:\Apps\Foo\*` {
		t.Errorf("remove must be case-insensitive: %v", p.AppLockerAllowPaths)
	}
}

func TestSetBlockEnabledPersists(t *testing.T) {
	m, store := newManager()
	addTestUser(t, m, "work1", "", "")
	if err := m.PublishPolicy(model.Policy{BlockEnabled: true}); err != nil {
		t.Fatal(err)
	}

	p, err := m.SetBlockEnabled(false)
	if err != nil {
		t.Fatal(err)
	}
	if p.BlockEnabled {
		t.Error("returned policy should have BlockEnabled = false")
	}

	data := store.objects[ossclient.PolicyKey()]
	var stored model.Policy
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.BlockEnabled {
		t.Error("stored policy should reflect the change")
	}
}

func TestSetSyncIntervalClamps(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "work1", "", "")
	if err := m.PublishPolicy(model.Policy{SyncIntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}

	p, err := m.SetSyncInterval(0)
	if err != nil {
		t.Fatal(err)
	}
	if p.SyncIntervalMinutes != 30 {
		t.Errorf("interval 0 should fall back to the existing value, got %d", p.SyncIntervalMinutes)
	}

	p, err = m.SetSyncInterval(99999)
	if err != nil {
		t.Fatal(err)
	}
	if p.SyncIntervalMinutes != model.MaxSyncInterval {
		t.Errorf("interval 99999 should clamp to %d, got %d", model.MaxSyncInterval, p.SyncIntervalMinutes)
	}
}

func TestCurrentPolicyDefaultsWhenNoneReadable(t *testing.T) {
	m, _ := newManager()
	p, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	want := model.DefaultPolicy()
	if p.BlockEnabled != want.BlockEnabled || len(p.BlockedDomains) != len(want.BlockedDomains) {
		t.Errorf("CurrentPolicy = %+v, want default-shaped policy", p)
	}
}

func TestCurrentPolicyReadsFirstEnabledUser(t *testing.T) {
	m, store := newManager()
	addTestUser(t, m, "work1", "", "")
	addTestUser(t, m, "work2", "", "")
	if err := m.SetUserEnabled("work1", false); err != nil {
		t.Fatal(err)
	}
	pol := model.Policy{BlockEnabled: false, BlockedDomains: []string{"x.com"}, SyncIntervalMinutes: 7}
	data, err := json.Marshal(pol)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[ossclient.PolicyKey()] = data

	got, err := m.CurrentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncIntervalMinutes != 7 || len(got.BlockedDomains) != 1 {
		t.Errorf("CurrentPolicy = %+v, want what was published", got)
	}
}

// The admin writes these objects and the agent reads them. They used to build
// the keys independently, which meant a change to the bucket layout could
// silently leave the admin publishing where nobody was looking. Both sides go
// through ossclient now; this keeps it that way.
func TestAdminWritesWhereTheAgentReads(t *testing.T) {
	for _, user := range []string{"work1", "Work2", "a-b_c"} {
		if got, want := credsKey(user), ossclient.UserKey(user, "credentials.zip"); got != want {
			t.Errorf("credsKey(%q) = %q, but the agent reads %q", user, got, want)
		}
	}
}

// A user name reaching the admin from a roster file must not be able to write
// outside the project's directory either.
func TestAdminKeysStayUnderRoot(t *testing.T) {
	for _, user := range []string{"work1", "../escape", "../../admin", ""} {
		if key := credsKey(user); !strings.HasPrefix(key, ossclient.Root) {
			t.Errorf("key %q for user %q is outside %s", key, user, ossclient.Root)
		}
	}
}

// Disabling a user must actually revoke: the flag alone changes nothing on
// the machines, because agents cannot read the roster. Deleting the stored
// credentials is what reaches them.
func TestDisableUserRevokesStoredCredentials(t *testing.T) {
	m, store := newManager()
	addTestUser(t, m, "work1", "", "")
	if err := m.PublishCredentials("work1", model.CredentialSet{
		model.PathCodexAuth: []byte(`{"tokens":{}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.objects[ossclient.UserKey("work1", "credentials.zip")]; !ok {
		t.Fatal("setup: credentials should exist before disabling")
	}

	if err := m.SetUserEnabled("work1", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.objects[ossclient.UserKey("work1", "credentials.zip")]; ok {
		t.Error("disabling a user must delete their stored credentials")
	}
}

// Re-enabling must not resurrect anything: the credentials are gone and the
// administrator has to sign in again, which is the honest state after a
// revocation.
func TestEnablingAfterDisableDoesNotRestoreCredentials(t *testing.T) {
	m, store := newManager()
	addTestUser(t, m, "work1", "", "")
	if err := m.PublishCredentials("work1", model.CredentialSet{
		model.PathCodexAuth: []byte(`{"tokens":{}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetUserEnabled("work1", false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetUserEnabled("work1", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.objects[ossclient.UserKey("work1", "credentials.zip")]; ok {
		t.Error("re-enabling must not bring credentials back")
	}
}

// Windows account names are case-insensitive, so the roster must not end up
// holding two entries for one person.
func TestRosterMatchesUserNamesIgnoringCase(t *testing.T) {
	m, _ := newManager()
	addTestUser(t, m, "work1", "a@x.com", "")
	addTestUser(t, m, "WORK1", "b@x.com", "")
	us, err := m.LoadUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(us.Users) != 1 {
		t.Fatalf("roster holds %d entries, want 1: %+v", len(us.Users), us.Users)
	}
	if us.Users[0].CodexAccount != "b@x.com" {
		t.Errorf("the second add should have updated the existing entry, got %+v", us.Users[0])
	}
	if err := m.SetUserEnabled("Work1", false); err != nil {
		t.Errorf("SetUserEnabled should find the user regardless of case: %v", err)
	}
}

// A missing roster is the administrator's first run; anything else is a
// failure to read one that may well exist. Treating the two alike lets a
// transient OSS error during Onboard save a roster holding one person.
func TestLoadUsersReportsReadFailures(t *testing.T) {
	m, store := newManager()
	store.objects[ossclient.AdminKey("users.json")] = []byte(`{"users":[{"windowsUser":"alice","enabled":true}]}`)
	store.getErrFor = ossclient.AdminKey("users.json")
	store.getErr = errors.New("transient network failure")

	if _, err := m.LoadUsers(); err == nil {
		t.Fatal("a transient read failure must be reported, not read as an empty roster")
	}
}

func TestLoadUsersTreatsAMissingRosterAsEmpty(t *testing.T) {
	m, _ := newManager()
	us, err := m.LoadUsers()
	if err != nil || len(us.Users) != 0 {
		t.Fatalf("first run: %+v %v", us, err)
	}
}
