package ossclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

func TestClassifyVerify(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error // sentinel expected via errors.Is; nil means "passed through"
	}{
		{"wrong key id", oss.ServiceError{Code: "InvalidAccessKeyId"}, ErrBadCredentials},
		{"bad signature", oss.ServiceError{Code: "SignatureDoesNotMatch"}, ErrBadCredentials},
		{"access denied", oss.ServiceError{Code: "AccessDenied"}, ErrAccessDenied},
		{"other service error", oss.ServiceError{Code: "NoSuchBucket"}, nil},
		{"plain error", fmt.Errorf("dial tcp: timeout"), nil},
	}
	for _, c := range cases {
		got := classifyVerify(c.in)
		if c.want != nil {
			if !errors.Is(got, c.want) {
				t.Errorf("%s: classifyVerify=%v, want errors.Is %v", c.name, got, c.want)
			}
		} else {
			if errors.Is(got, ErrBadCredentials) || errors.Is(got, ErrAccessDenied) {
				t.Errorf("%s: classifyVerify=%v should have passed through unchanged", c.name, got)
			}
		}
	}
}

func TestUserKey(t *testing.T) {
	cases := []struct{ user, name, want string }{
		{"work1", "policy.json", Root + "work1/policy.json"},
		{"work2", "credentials.zip", Root + "work2/credentials.zip"},
		{"work3", "status.json", Root + "work3/status.json"},
	}
	for _, c := range cases {
		if got := UserKey(c.user, c.name); got != c.want {
			t.Errorf("UserKey(%q,%q) = %q, want %q", c.user, c.name, got, c.want)
		}
	}
}

// The user name comes from the local machine, so a crafted value must not be
// able to reach another employee's directory or the admin one. Note what
// happens to "../../admin": it does not escape, it just becomes a directory
// literally named "admin" inside Root -- which is a different place from the
// real admin/ directory at the bucket root, and one the agent's policy does
// not grant anything on either.
func TestUserKeyResistsTraversal(t *testing.T) {
	cases := []struct{ user, want string }{
		{"../other", Root + "other/policy.json"},
		{"../../admin", Root + "admin/policy.json"},
		{"../../admin/users.json", Root + "users.json/policy.json"},
		{`..\other`, Root + "other/policy.json"},
		{"a/b/c", Root + "c/policy.json"},
		{"/", Root + "_invalid/policy.json"},
		{"", Root + "_invalid/policy.json"},
		{".", Root + "_invalid/policy.json"},
	}
	for _, c := range cases {
		got := UserKey(c.user, "policy.json")
		if got != c.want {
			t.Errorf("UserKey(%q) = %q, want %q", c.user, got, c.want)
		}
		// Whatever the input, the result must stay one directory below Root.
		if n := segmentsBelow(got, Root); n != 2 {
			t.Errorf("UserKey(%q) = %q is %d levels below %s, want 2", c.user, got, n, Root)
		}
	}
}

// segmentsBelow counts how many path elements a key has under the given
// prefix. It returns -1 when the key is not under it at all.
func segmentsBelow(key, prefix string) int {
	if !strings.HasPrefix(key, prefix) {
		return -1
	}
	return len(strings.Split(strings.TrimPrefix(key, prefix), "/"))
}

func TestAdminKey(t *testing.T) {
	want := AdminRoot + "users.json"
	if got := AdminKey("users.json"); got != want {
		t.Errorf("AdminKey = %q, want %q", got, want)
	}
}

func TestNewRequiresAllFields(t *testing.T) {
	cases := []struct {
		name                               string
		endpoint, bucket, keyID, keySecret string
	}{
		{"missing endpoint", "", "bucket", "ak", "sk"},
		{"missing bucket", "oss-cn-hangzhou.aliyuncs.com", "", "ak", "sk"},
		{"missing key id", "oss-cn-hangzhou.aliyuncs.com", "bucket", "", "sk"},
		{"missing key secret", "oss-cn-hangzhou.aliyuncs.com", "bucket", "ak", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.endpoint, c.bucket, c.keyID, c.keySecret); err == nil {
				t.Error("expected an error for incomplete configuration")
			}
		})
	}
}

func TestNewAcceptsCompleteConfig(t *testing.T) {
	// Construction is offline; no network call happens until a method is used.
	c, err := New("oss-cn-hangzhou.aliyuncs.com", "bucket", "ak", "sk")
	if err != nil {
		t.Fatalf("New with a complete config: %v", err)
	}
	if c == nil {
		t.Fatal("New returned a nil client without an error")
	}
}

func TestBindingAndStatusKeys(t *testing.T) {
	if want := Root + "_bindings/DESKTOP-A.json"; BindingKey("DESKTOP-A") != want {
		t.Errorf("BindingKey = %q, want %q", BindingKey("DESKTOP-A"), want)
	}
	if want := Root + "_status/DESKTOP-A.json"; StatusKey("DESKTOP-A") != want {
		t.Errorf("StatusKey = %q, want %q", StatusKey("DESKTOP-A"), want)
	}
}

// Hostnames come from the machine itself, so they get the same treatment as
// user names: whatever arrives must stay inside its own prefix.
func TestMachineKeysResistTraversal(t *testing.T) {
	for _, machine := range []string{"../work1", "../../_admin", `..\evil`, "a/b/c"} {
		b := BindingKey(machine)
		s := StatusKey(machine)
		if segmentsBelow(b, BindingPrefix) != 1 {
			t.Errorf("BindingKey(%q) = %q escapes its prefix", machine, b)
		}
		if segmentsBelow(s, StatusPrefix) != 1 {
			t.Errorf("StatusKey(%q) = %q escapes its prefix", machine, s)
		}
	}
}

func TestMachineFromKeys(t *testing.T) {
	if got := MachineFromStatusKey(Root + "_status/DESKTOP-A.json"); got != "DESKTOP-A" {
		t.Errorf("MachineFromStatusKey = %q", got)
	}
	if got := MachineFromBindingKey(Root + "_bindings/DESKTOP-B.json"); got != "DESKTOP-B" {
		t.Errorf("MachineFromBindingKey = %q", got)
	}
}

func TestKeyRoundTrip(t *testing.T) {
	for _, machine := range []string{"DESKTOP-A", "iZt4n2up5vmbxv0bvvnp1oZ", "PC_01"} {
		if got := MachineFromStatusKey(StatusKey(machine)); got != machine {
			t.Errorf("round trip lost %q, got %q", machine, got)
		}
		if got := MachineFromBindingKey(BindingKey(machine)); got != machine {
			t.Errorf("round trip lost %q, got %q", machine, got)
		}
	}
}

// Everything the agent touches must stay under Root: the agent's RAM policy
// is scoped to that one path, and the bucket is allowed to hold unrelated
// data beside it.
func TestAgentKeysStayUnderRoot(t *testing.T) {
	keys := []string{
		UserKey("work1", "policy.json"),
		UserKey("../escape", "credentials.zip"),
		UserKey("", "policy.json"),
		BindingKey("DESKTOP-A"),
		BindingKey("../../escape"),
		StatusKey("DESKTOP-A"),
		StatusKey(`..\escape`),
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, Root) {
			t.Errorf("key %q is outside %s", k, Root)
		}
	}
	for _, p := range []string{BindingPrefix, StatusPrefix} {
		if !strings.HasPrefix(p, Root) {
			t.Errorf("prefix %q is outside %s", p, Root)
		}
	}
}

// The roster is the one thing no agent may read. It lives outside Root so
// that the agent's authorisation cannot reach it even by accident -- no
// prefix exception to get wrong.
func TestAdminKeysAreOutsideTheAgentDirectory(t *testing.T) {
	key := AdminKey("users.json")
	if strings.HasPrefix(key, Root) {
		t.Errorf("admin key %q is inside the agent's directory %s", key, Root)
	}
	if !strings.HasPrefix(key, AdminRoot) {
		t.Errorf("admin key %q is not under %s", key, AdminRoot)
	}
	// The two must not be nested either way round, or a wildcard grant on
	// one would cover the other.
	if strings.HasPrefix(AdminRoot, Root) || strings.HasPrefix(Root, AdminRoot) {
		t.Errorf("%q and %q must be siblings, not nested", Root, AdminRoot)
	}
}

// Everything belonging to one employee lives under that employee's own
// directory, so a single prefix covers the whole person -- which is what
// makes per-employee lifecycle rules and per-employee cleanup possible.
func TestOneEmployeeIsOnePrefix(t *testing.T) {
	const user = "work1"
	prefix := UserPrefix(user)

	for _, key := range []string{
		UserKey(user, "policy.json"),
		UserKey(user, "credentials.zip"),
		DataCollectPrefix(user) + "2026-08-04/session-1.jsonl",
	} {
		if !strings.HasPrefix(key, prefix) {
			t.Errorf("key %q is outside %s", key, prefix)
		}
	}

	// And one employee's prefix must not swallow another's.
	if strings.HasPrefix(UserPrefix("work2"), prefix) {
		t.Errorf("%s is nested inside %s", UserPrefix("work2"), prefix)
	}
}

// A crafted user name must not let collected data escape into another
// employee's directory either.
func TestDataCollectResistsTraversal(t *testing.T) {
	for _, user := range []string{"../work2", "../../admin", `..\evil`, "a/b/c", ""} {
		got := DataCollectPrefix(user)
		if !strings.HasPrefix(got, Root) {
			t.Errorf("DataCollectPrefix(%q) = %q is outside %s", user, got, Root)
		}
		// Root + one user segment + data_collect/ and nothing more.
		if n := segmentsBelow(got, Root); n != 3 {
			t.Errorf("DataCollectPrefix(%q) = %q is %d levels below %s, want 3", user, got, n, Root)
		}
	}
}

// The SDK defaults a bare hostname to plain http. Everything this project
// moves through OSS is sensitive -- credentials.zip holds live OAuth tokens --
// so a scheme-less endpoint must come out as TLS.
func TestBareEndpointBecomesHTTPS(t *testing.T) {
	cases := map[string]string{
		"oss-ap-southeast-1.aliyuncs.com":       "https://oss-ap-southeast-1.aliyuncs.com",
		"oss-cn-hangzhou-internal.aliyuncs.com": "https://oss-cn-hangzhou-internal.aliyuncs.com",
		"https://oss-cn-hangzhou.aliyuncs.com":  "https://oss-cn-hangzhou.aliyuncs.com",
		"http://oss-cn-hangzhou.aliyuncs.com":   "http://oss-cn-hangzhou.aliyuncs.com",
	}
	for in, want := range cases {
		if got := withHTTPS(in); got != want {
			t.Errorf("withHTTPS(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDataCollectKey(t *testing.T) {
	cases := []struct{ user, rel, want string }{
		{"work1", ".claude/projects/p/a.jsonl", "agent_workdir/work1/data_collect/.claude/projects/p/a.jsonl"},
		{"work1", ".codex/sessions/2026/rollout-x.jsonl", "agent_workdir/work1/data_collect/.codex/sessions/2026/rollout-x.jsonl"},
		{"work1", `.claude\projects\a.jsonl`, "agent_workdir/work1/data_collect/.claude/projects/a.jsonl"}, // 反斜杠归一
		{"work1", "../../etc/passwd", "agent_workdir/work1/data_collect/etc/passwd"},                       // .. 不得逃逸
	}
	for _, c := range cases {
		if got := DataCollectKey(c.user, c.rel); got != c.want {
			t.Errorf("DataCollectKey(%q,%q)=%q want %q", c.user, c.rel, got, c.want)
		}
	}
}

func TestDataCollectUser(t *testing.T) {
	cases := []struct {
		key       string
		user, rel string
		ok        bool
	}{
		{"agent_workdir/work1/data_collect/.claude/projects/p/a.jsonl", "work1", ".claude/projects/p/a.jsonl", true},
		{"agent_workdir/peter/data_collect/.codex/sessions/x.jsonl", "peter", ".codex/sessions/x.jsonl", true},
		// not data_collect objects
		{"agent_workdir/work1/credentials.zip", "", "", false},
		{"agent_workdir/policy.json", "", "", false},
		{"agent_workdir/_status/DESKTOP-A.json", "", "", false},
		{"admin/users.json", "", "", false},
	}
	for _, c := range cases {
		user, rel, ok := DataCollectUser(c.key)
		if ok != c.ok || user != c.user || rel != c.rel {
			t.Errorf("DataCollectUser(%q) = (%q,%q,%v) want (%q,%q,%v)", c.key, user, rel, ok, c.user, c.rel, c.ok)
		}
	}
}

// DataCollectUser must be the exact inverse of DataCollectKey for real inputs.
func TestDataCollectUserRoundTrip(t *testing.T) {
	user, rel := "weipeng", ".claude/projects/p/a.jsonl"
	key := DataCollectKey(user, rel)
	gotUser, gotRel, ok := DataCollectUser(key)
	if !ok || gotUser != user || gotRel != rel {
		t.Fatalf("round trip: (%q,%q,%v) want (%q,%q,true)", gotUser, gotRel, ok, user, rel)
	}
}
