package admincore

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
)

func TestQuotaDefaultsFallBackToBuiltIn(t *testing.T) {
	m, _ := newManager()
	q, err := m.LoadQuotaDefaults()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if q != DefaultQuota {
		t.Errorf("empty store should yield the built-in defaults, got %+v", q)
	}
}

func TestQuotaDefaultsRoundTrip(t *testing.T) {
	m, _ := newManager()
	want := litellm.Quota{MonthlyBudgetUSD: 35, RPM: 90, TPM: 300000, Parallel: 6}
	if err := m.SaveQuotaDefaults(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := m.LoadQuotaDefaults()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestQuotaDefaultsRejectInvalid(t *testing.T) {
	m, store := newManager()
	if err := m.SaveQuotaDefaults(litellm.Quota{MonthlyBudgetUSD: 0, RPM: 1, TPM: 1, Parallel: 1}); err == nil {
		t.Fatal("zero budget must be refused")
	}
	if _, ok := store.objects[QuotaDefaultsKey()]; ok {
		t.Error("invalid defaults must not be written")
	}
}
