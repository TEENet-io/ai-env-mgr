package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPolicyRoundTrip(t *testing.T) {
	raw := `{"blockEnabled":true,"blockedDomains":["openai.com","claude.ai"],"syncIntervalMinutes":15,"updatedAt":"2026-08-03T16:20:00Z"}`
	var p Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !p.BlockEnabled {
		t.Error("BlockEnabled should be true")
	}
	if len(p.BlockedDomains) != 2 || p.BlockedDomains[0] != "openai.com" {
		t.Errorf("BlockedDomains = %v", p.BlockedDomains)
	}
	if p.SyncIntervalMinutes != 15 {
		t.Errorf("SyncIntervalMinutes = %d, want 15", p.SyncIntervalMinutes)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again Policy
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if again.UpdatedAt != p.UpdatedAt {
		t.Errorf("UpdatedAt lost: %q vs %q", again.UpdatedAt, p.UpdatedAt)
	}
}

func TestClampInterval(t *testing.T) {
	cases := []struct {
		name                string
		requested, fallback int
		want                int
	}{
		{"normal value kept", 15, 30, 15},
		{"zero falls back", 0, 20, 20},
		{"negative falls back", -5, 20, 20},
		{"both unset uses default", 0, 0, DefaultSyncInterval},
		{"below minimum clamped up", 0, -1, DefaultSyncInterval},
		{"above maximum clamped down", 99999, 30, MaxSyncInterval},
		{"exactly minimum kept", 1, 30, 1},
		{"exactly maximum kept", 1440, 30, 1440},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClampInterval(c.requested, c.fallback); got != c.want {
				t.Errorf("ClampInterval(%d,%d) = %d, want %d", c.requested, c.fallback, got, c.want)
			}
		})
	}
}

func TestStatusHasRequiredFields(t *testing.T) {
	s := Status{
		Machine: "DESKTOP-A", BoundUser: "work1",
		LocalUsers: []string{"Administrator", "work1"}, BoundUserExists: true,
		AgentVersion: "1.0.0", PolicyETag: "abc", CredsETag: "def",
		BlockEnabled: true, BlockedDomains: 4, SyncIntervalMinutes: 30,
		CredsApplied: true, AppLockerMode: "Enforce", Errors: []string{},
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, k := range []string{
		"machine", "boundUser", "localUsers", "boundUserExists",
		"lastSync", "agentVersion", "policyEtag", "credsEtag",
		"blockEnabled", "blockedDomains", "syncIntervalMinutes", "credsApplied",
		"appLockerMode", "errors",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in status json", k)
		}
	}
}

func TestUsersLookup(t *testing.T) {
	us := Users{Users: []UserEntry{
		{WindowsUser: "work1", Enabled: true},
		{WindowsUser: "work2", Enabled: false},
	}}
	if e := us.Find("work1"); e == nil || !e.Enabled {
		t.Error("work1 should be found and enabled")
	}
	if e := us.Find("work2"); e == nil || e.Enabled {
		t.Error("work2 should be found and disabled")
	}
	if us.Find("nobody") != nil {
		t.Error("unknown user should return nil")
	}
}

func TestUsersFindReturnsMutablePointer(t *testing.T) {
	us := Users{Users: []UserEntry{{WindowsUser: "work1", Enabled: true}}}
	us.Find("work1").Enabled = false
	if us.Users[0].Enabled {
		t.Error("Find must return a pointer into the slice so edits persist")
	}
}

func TestDefaultPolicyIsUsable(t *testing.T) {
	p := DefaultPolicy()
	if !p.BlockEnabled {
		t.Error("default policy should block")
	}
	if len(p.BlockedDomains) == 0 {
		t.Error("default policy should list domains")
	}
	if p.SyncIntervalMinutes != DefaultSyncInterval {
		t.Errorf("SyncIntervalMinutes = %d, want %d", p.SyncIntervalMinutes, DefaultSyncInterval)
	}
	if p.UpdatedAt == "" {
		t.Error("UpdatedAt should be stamped")
	}
}

func TestStatusHasLocalUser(t *testing.T) {
	s := Status{LocalUsers: []string{"Administrator", "work1"}}
	if !s.HasLocalUser("work1") {
		t.Error("work1 should be found")
	}
	// Windows account names are case-insensitive.
	if !s.HasLocalUser("WORK1") {
		t.Error("lookup should ignore case")
	}
	if s.HasLocalUser("work2") {
		t.Error("work2 should not be found")
	}
	if (Status{}).HasLocalUser("anyone") {
		t.Error("an empty list should match nothing")
	}
}

func TestBindingRoundTrip(t *testing.T) {
	raw := `{"user":"work1","boundAt":"2026-08-04T10:00:00Z","note":"dev team"}`
	var b Binding
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.User != "work1" || b.Note != "dev team" {
		t.Errorf("binding not parsed: %+v", b)
	}
	// note is optional and must not appear when empty
	out, err := json.Marshal(Binding{User: "work2", BoundAt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "note") {
		t.Errorf("empty note should be omitted, got %s", out)
	}
}
