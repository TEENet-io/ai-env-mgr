package dbstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestSetAndGetQuota(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")

	stored, err := s.Quotas().Set(ctx, e.ID, repo.Quota{
		MonthlyBudget: "12.5", RPM: 60, TPM: 2000000, Parallel: 8,
	}, 0)
	if err != nil {
		t.Fatalf("set quota: %v", err)
	}
	// numeric(12,6) round-trips the amount exactly, in its own scale. What
	// matters is that nothing was lost: a float column would have stored
	// 12.499999999999998 for some perfectly ordinary budgets.
	if stored.MonthlyBudget != "12.500000" {
		t.Errorf("monthly budget = %q, want 12.500000", stored.MonthlyBudget)
	}
	if stored.Currency != repo.DefaultCurrency || stored.PeriodRule != repo.PeriodCalendarMonthUTC ||
		stored.PeriodTZ != repo.DefaultPeriodTZ {
		t.Errorf("defaults not filled in: %+v", stored)
	}
	if stored.Version != 1 {
		t.Errorf("version = %d, want 1", stored.Version)
	}

	got, err := s.Quotas().Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if got != stored {
		t.Errorf("Get returned %+v, want %+v", got, stored)
	}
}

func TestSetQuotaRefusesToOverwriteBlind(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	base := repo.Quota{MonthlyBudget: "50", RPM: 60, TPM: 2000000, Parallel: 8}

	first, err := s.Quotas().Set(ctx, e.ID, base, 0)
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	// "There is no quota yet" is a claim about what the caller read. Being
	// wrong about it must not discard limits somebody set on purpose.
	raised := base
	raised.MonthlyBudget = "500"
	if _, err := s.Quotas().Set(ctx, e.ID, raised, 0); !errors.Is(err, repo.ErrConflict) {
		t.Errorf("creating over an existing quota: error = %v, want ErrConflict", err)
	}
	if _, err := s.Quotas().Set(ctx, e.ID, raised, first.Version+7); !errors.Is(err, repo.ErrConflict) {
		t.Errorf("stale version: error = %v, want ErrConflict", err)
	}
	unchanged, err := s.Quotas().Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if unchanged.MonthlyBudget != "50.000000" {
		t.Errorf("budget = %q; a refused write went through", unchanged.MonthlyBudget)
	}

	updated, err := s.Quotas().Set(ctx, e.ID, raised, first.Version)
	if err != nil {
		t.Fatalf("set with the right version: %v", err)
	}
	if updated.MonthlyBudget != "500.000000" || updated.Version != first.Version+1 {
		t.Errorf("updated = %+v, want 500 at version %d", updated, first.Version+1)
	}
}

func TestSetQuotaRejectsValuesThatWouldBlockSomebody(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	ok := repo.Quota{MonthlyBudget: "50", RPM: 60, TPM: 2000000, Parallel: 8}

	cases := []struct {
		name  string
		quota repo.Quota
		want  string
	}{
		// The gateway treats a zero budget as "blocked immediately", which is
		// never what somebody filling in a form meant.
		{"zero budget", with(ok, func(q *repo.Quota) { q.MonthlyBudget = "0" }), "greater than zero"},
		{"zero budget with decimals", with(ok, func(q *repo.Quota) { q.MonthlyBudget = "0.000" }), "greater than zero"},
		{"negative budget", with(ok, func(q *repo.Quota) { q.MonthlyBudget = "-5" }), "plain decimal"},
		{"exponent", with(ok, func(q *repo.Quota) { q.MonthlyBudget = "1e3" }), "plain decimal"},
		{"currency in the amount", with(ok, func(q *repo.Quota) { q.MonthlyBudget = "50 USD" }), "plain decimal"},
		{"empty budget", with(ok, func(q *repo.Quota) { q.MonthlyBudget = "" }), "plain decimal"},
		{"more precision than the column", with(ok, func(q *repo.Quota) { q.MonthlyBudget = "0.1234567" }), "plain decimal"},
		{"no requests", with(ok, func(q *repo.Quota) { q.RPM = 0 }), "requests per minute"},
		{"no tokens", with(ok, func(q *repo.Quota) { q.TPM = -1 }), "tokens per minute"},
		{"no concurrency", with(ok, func(q *repo.Quota) { q.Parallel = 0 }), "parallel"},
		// Storing a period rule the gateway does not implement would be a
		// promise the console cannot keep.
		{"unsupported period", with(ok, func(q *repo.Quota) { q.PeriodRule = "rolling_30d" }), "period rule"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Quotas().Set(ctx, e.ID, c.quota, 0)
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
	if _, err := s.Quotas().Get(ctx, e.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("a rejected quota was stored anyway: %v", err)
	}
}

func TestQuotaForAnUnknownEmployee(t *testing.T) {
	s, ctx := newTestStore(t)
	missing := "6f1e9e6c-0000-4000-8000-000000000000"

	if _, err := s.Quotas().Get(ctx, missing); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("Get: error = %v, want ErrNotFound", err)
	}
	_, err := s.Quotas().Set(ctx, missing, repo.Quota{
		MonthlyBudget: "50", RPM: 60, TPM: 2000000, Parallel: 8,
	}, 0)
	if !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("Set: error = %v, want ErrNotFound", err)
	}
}

func with(q repo.Quota, edit func(*repo.Quota)) repo.Quota {
	edit(&q)
	return q
}
