package dbstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestSettingsRoundTripAndVersion(t *testing.T) {
	s, ctx := newTestStore(t)

	// Nothing saved yet is the one answer a caller may turn into the built-in
	// default. Anything else means the database could not be read, and opening
	// an account on a budget nobody chose is the failure this distinction
	// exists to prevent.
	if _, err := s.Settings().Get(ctx, repo.SettingQuotaDefaults); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("Get before anything is saved: error = %v, want ErrNotFound", err)
	}

	defaults := []byte(`{"monthlyBudgetUSD":50,"rpm":60,"tpm":2000000,"parallel":8}`)
	saved, err := s.Settings().Set(ctx, repo.SettingQuotaDefaults, defaults, 0, "admin")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.Version != 1 || saved.UpdatedBy != "admin" {
		t.Errorf("saved = %+v, want version 1 saved by admin", saved)
	}

	got, err := s.Settings().Get(ctx, repo.SettingQuotaDefaults)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(string(got.Value), `"tpm": 2000000`) &&
		!strings.Contains(string(got.Value), `"tpm":2000000`) {
		t.Errorf("stored value = %s, want the numbers as saved", got.Value)
	}

	raised := []byte(`{"monthlyBudgetUSD":100,"rpm":60,"tpm":2000000,"parallel":8}`)
	if _, err := s.Settings().Set(ctx, repo.SettingQuotaDefaults, raised, 0, "admin"); !errors.Is(err, repo.ErrConflict) {
		t.Errorf("saving as if nothing were stored: error = %v, want ErrConflict", err)
	}
	if _, err := s.Settings().Set(ctx, repo.SettingQuotaDefaults, raised, 99, "admin"); !errors.Is(err, repo.ErrConflict) {
		t.Errorf("saving with a stale version: error = %v, want ErrConflict", err)
	}
	updated, err := s.Settings().Set(ctx, repo.SettingQuotaDefaults, raised, saved.Version, "someone else")
	if err != nil {
		t.Fatalf("save with the right version: %v", err)
	}
	if updated.Version != saved.Version+1 || updated.UpdatedBy != "someone else" {
		t.Errorf("updated = %+v, want version %d by someone else", updated, saved.Version+1)
	}
}

func TestSettingsRejectBadInput(t *testing.T) {
	s, ctx := newTestStore(t)

	if _, err := s.Settings().Set(ctx, "", []byte(`{}`), 0, "admin"); err == nil {
		t.Error("a setting with no key was saved")
	}
	// The column is jsonb and would reject this anyway; failing here names the
	// caller's mistake instead of a parser position.
	if _, err := s.Settings().Set(ctx, "broken", []byte(`{"a":`), 0, "admin"); err == nil {
		t.Error("a truncated JSON value was saved")
	}
	if _, err := s.Settings().Get(ctx, "broken"); !errors.Is(err, repo.ErrNotFound) {
		t.Error("the rejected value was stored anyway")
	}
}

func TestSettingsList(t *testing.T) {
	s, ctx := newTestStore(t)
	for _, key := range []string{"zebra", "alpha"} {
		if _, err := s.Settings().Set(ctx, key, []byte(`{"on":true}`), 0, "admin"); err != nil {
			t.Fatalf("save %s: %v", key, err)
		}
	}
	list, err := s.Settings().List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Key != "alpha" || list[1].Key != "zebra" {
		t.Errorf("list = %+v, want both settings by key", list)
	}
}
