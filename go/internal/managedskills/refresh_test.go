package managedskills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func TestDesktopAckIsRequiredAndOldAckCannotConfirmRollback(t *testing.T) {
	m := manager(t)
	ack := ""
	m.Refresh = func(user, generation string) (bool, error) { return generation == ack, nil }
	plan := model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle(1, "first")}}
	first, err := m.Sync(plan)
	if err != nil || first.State != "refresh_pending" || len(first.RefreshPending) != 1 {
		t.Fatalf("initial pending: %+v %v", first, err)
	}
	again, err := m.Sync(plan)
	if err != nil || again.Generation != first.Generation {
		t.Fatal("unchanged generation was not stable")
	}
	ack = first.Generation
	if st, err := m.Sync(plan); err != nil || st.State != "applied" {
		t.Fatalf("ack not applied: %+v %v", st, err)
	}
	plan.Skills[0] = bundle(2, "second")
	second, err := m.Sync(plan)
	if err != nil || second.State != "refresh_pending" || second.Generation == first.Generation {
		t.Fatalf("update accepted stale ack: %+v %v", second, err)
	}
	plan.Skills[0] = bundle(1, "first")
	rollback, err := m.Sync(plan)
	if err != nil || rollback.State != "refresh_pending" || rollback.Generation == first.Generation || rollback.Generation == second.Generation {
		t.Fatalf("rollback reused ack: %+v %v", rollback, err)
	}
	revoked, err := m.Sync(model.ManagedSkills{})
	if err != nil || revoked.State != "refresh_pending" {
		t.Fatalf("revocation: %+v %v", revoked, err)
	}
	data, err := os.ReadFile(filepath.Join(m.Root, RefreshManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest RefreshManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Skills) != 0 || len(manifest.Users) != 1 || manifest.Users[0] != "work1" || manifest.Generation != revoked.Generation {
		t.Fatalf("revocation signal: %+v", manifest)
	}
	ack = revoked.Generation
	copy := *m
	if st, err := copy.Sync(model.ManagedSkills{}); err != nil || st.State != "applied" || st.Generation != revoked.Generation {
		t.Fatalf("restart ack: %+v %v", st, err)
	}
}

func TestDesktopAckChecksGenerationUserSizeAndLinks(t *testing.T) {
	profile := t.TempDir()
	file := filepath.Join(profile, "AppData", "Local", "TEENet", "company-skills-refresh.json")
	if ok, err := DesktopRefreshed(profile, "work1", "new"); ok || err != nil {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		generation, user string
		want             bool
	}{{"old", "work1", false}, {"new", "other", false}, {"new", "WORK1", true}} {
		data, _ := json.Marshal(map[string]any{"version": 1, "generation": tc.generation, "user": tc.user})
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		if ok, err := DesktopRefreshed(profile, "work1", "new"); err != nil || ok != tc.want {
			t.Fatalf("ack %+v: %v %v", tc, ok, err)
		}
	}
	if err := os.WriteFile(file, make([]byte, 4097), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, err := DesktopRefreshed(profile, "work1", "new"); ok || err == nil {
		t.Fatal("oversized ack accepted")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "ack.json")
	if err := os.WriteFile(outside, []byte(`{"version":1,"generation":"new","user":"work1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Skip("symlink not available")
	}
	if ok, err := DesktopRefreshed(profile, "work1", "new"); ok || err == nil {
		t.Fatal("linked ack accepted")
	}
}
