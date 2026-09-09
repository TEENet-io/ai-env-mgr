package status

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func TestBuildPopulatesFields(t *testing.T) {
	p := model.Policy{BlockEnabled: true, BlockedDomains: []string{"a.com", "b.com"},
		AppLockerAllowPaths: []string{`C:\tools\Codex\*`}}
	s := Build(Report{
		Machine: "DESKTOP-A", BoundUser: "work1",
		LocalUsers: []string{"Administrator", "work1"}, BoundUserExists: true,
		Version: "1.2.3", Policy: p,
		PolicyETag: "petag", CredsETag: "cetag",
		Interval: 15, CredsApplied: true,
	})

	if s.Machine != "DESKTOP-A" || s.BoundUser != "work1" || s.AgentVersion != "1.2.3" {
		t.Errorf("identity fields wrong: %+v", s)
	}
	if len(s.LocalUsers) != 2 || !s.BoundUserExists {
		t.Errorf("local user info wrong: %+v", s)
	}
	if s.PolicyETag != "petag" || s.CredsETag != "cetag" {
		t.Errorf("etags wrong: %+v", s)
	}
	if !s.BlockEnabled || s.BlockedDomains != 2 {
		t.Errorf("policy summary wrong: %+v", s)
	}
	if s.AppLockerAllowPaths != 1 {
		t.Errorf("AppLockerAllowPaths = %d, want 1", s.AppLockerAllowPaths)
	}
	if s.SyncIntervalMinutes != 15 {
		t.Errorf("SyncIntervalMinutes = %d, want 15", s.SyncIntervalMinutes)
	}
	if !s.CredsApplied {
		t.Error("CredsApplied should be true")
	}
	if s.AppLockerMode == "" {
		t.Error("AppLockerMode should be filled in")
	}
	if _, err := time.Parse(time.RFC3339, s.LastSync); err != nil {
		t.Errorf("LastSync %q is not RFC3339: %v", s.LastSync, err)
	}
}

func TestBuildCarriesErrors(t *testing.T) {
	s := Build(Report{Machine: "M", BoundUser: "work1", Version: "1.0.0", Interval: 30, Errors: []string{"policy: boom", "creds: bang"}})
	if len(s.Errors) != 2 {
		t.Fatalf("got %d errors, want 2", len(s.Errors))
	}
	if s.Errors[0] != "policy: boom" {
		t.Errorf("Errors[0] = %q", s.Errors[0])
	}
}

// The admin iterates over errors; a nil slice would marshal to null and break
// that loop, so Build must normalise it to an empty array.
func TestMarshalEmitsEmptyArrayNotNull(t *testing.T) {
	s := Build(Report{Machine: "M", BoundUser: "work1", Version: "1.0.0", Interval: 30})
	out, err := Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	arr, ok := m["errors"].([]any)
	if !ok {
		t.Fatalf("errors is %T, want array", m["errors"])
	}
	if len(arr) != 0 {
		t.Errorf("errors = %v, want empty", arr)
	}
}

func TestMarshalParseRoundTrip(t *testing.T) {
	p := model.Policy{BlockEnabled: true, BlockedDomains: []string{"a.com"}}
	s := Build(Report{
		Machine: "DESKTOP-A", BoundUser: "work1", Version: "1.0.0", Policy: p,
		PolicyETag: "petag", CredsETag: "cetag", Interval: 45,
		CredsApplied: true, Errors: []string{"oops"},
	})

	out, err := Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if back.Machine != s.Machine || back.BoundUser != s.BoundUser || back.PolicyETag != s.PolicyETag ||
		back.SyncIntervalMinutes != s.SyncIntervalMinutes || len(back.Errors) != 1 {
		t.Errorf("round trip lost data:\n got %+v\nwant %+v", back, s)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Error("parsing garbage should fail")
	}
}

func TestAgeReportsElapsedTime(t *testing.T) {
	s := model.Status{LastSync: time.Now().UTC().Add(-90 * time.Minute).Format(time.RFC3339)}
	age := Age(s)
	if age < 89*time.Minute || age > 91*time.Minute {
		t.Errorf("Age = %v, want about 90m", age)
	}
}

func TestAgeOnUnparseableTimestamp(t *testing.T) {
	got := Age(model.Status{LastSync: "garbage"})
	if got < 24*time.Hour {
		t.Errorf("Age on a bad timestamp = %v, want a very large duration", got)
	}
}

func TestAgeOnEmptyTimestamp(t *testing.T) {
	if got := Age(model.Status{}); got < 24*time.Hour {
		t.Errorf("Age on a missing timestamp = %v, want a very large duration", got)
	}
}

// A machine whose clock runs ahead would otherwise report a negative age and
// look permanently fresh, hiding the fact that it stopped syncing.
func TestAgeOnFutureTimestamp(t *testing.T) {
	s := model.Status{LastSync: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)}
	if got := Age(s); got < 24*time.Hour {
		t.Errorf("Age on a future timestamp = %v, want a very large duration", got)
	}
}

// The admin needs an array to iterate, not null, even on a machine that has
// no local users or no binding yet.
func TestBuildNormalisesNilSlices(t *testing.T) {
	s := Build(Report{Machine: "DESKTOP-C"})
	if s.LocalUsers == nil {
		t.Error("LocalUsers must be an empty slice, not nil")
	}
	if s.Errors == nil {
		t.Error("Errors must be an empty slice, not nil")
	}
}
