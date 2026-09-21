package dbstore

import (
	"errors"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestUsageUpsertReplacesTheDayAndSums(t *testing.T) {
	s, ctx := newTestStore(t)
	work1 := mustCreate(t, ctx, s, "work1")
	work2 := mustCreate(t, ctx, s, "work2")
	d := func(day int) time.Time { return time.Date(2026, 9, day, 15, 30, 0, 0, time.FixedZone("CST", 8*3600)) }

	if _, _, err := s.Usage().Days(ctx); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("empty table: Days err = %v, want ErrNotFound", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Usage().UpsertDay(ctx, d(1), []repo.UsageRow{
		{EmployeeID: work1.ID, KeyAlias: "emp-work1-e1", ModelGroup: "sonnet", Calls: 10, Failures: 1, CostUSD: "1.5", PromptTokens: 100, CompletionTokens: 50},
		{EmployeeID: work1.ID, KeyAlias: "emp-work1-e1", ModelGroup: "opus", Calls: 2, CostUSD: "4", Unpriced: 1},
		{EmployeeID: work2.ID, KeyAlias: "emp-work2-e1", ModelGroup: "sonnet", Calls: 5, CostUSD: "0.5"},
	}))
	must(s.Usage().UpsertDay(ctx, d(2), []repo.UsageRow{
		{EmployeeID: work1.ID, KeyAlias: "emp-work1-e1", ModelGroup: "sonnet", Calls: 1, CostUSD: "0.25"},
		{KeyAlias: "sk-unknown", ModelGroup: "sonnet", Calls: 3, CostUSD: "0.75"},
	}))
	// A second snapshot of day 1 replaces it rather than adding to it.
	must(s.Usage().UpsertDay(ctx, d(1), []repo.UsageRow{
		{EmployeeID: work1.ID, KeyAlias: "emp-work1-e1", ModelGroup: "sonnet", Calls: 12, Failures: 2, CostUSD: "2", PromptTokens: 120, CompletionTokens: 60},
		{EmployeeID: work2.ID, KeyAlias: "emp-work2-e1", ModelGroup: "sonnet", Calls: 5, CostUSD: "0.5"},
	}))

	month := repo.UsageFilter{From: d(1), To: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	byEmp, err := s.Usage().ByEmployee(ctx, month)
	must(err)
	if len(byEmp) != 3 || byEmp[0].EmployeeID != work1.ID || byEmp[0].Calls != 13 || byEmp[0].Failures != 2 ||
		byEmp[0].CostUSD != "2.250000" || byEmp[0].PromptTokens != 120 || byEmp[0].CompletionTokens != 60 {
		t.Fatalf("by employee = %+v", byEmp)
	}
	// Cost order: work1 2.25, the unresolved alias 0.75, work2 0.5.
	if byEmp[1].EmployeeID != "" || byEmp[1].KeyAlias != "sk-unknown" || byEmp[1].Calls != 3 || byEmp[2].EmployeeID != work2.ID {
		t.Fatalf("the unresolved alias is its own line, ordered by cost: %+v", byEmp)
	}
	byModel, err := s.Usage().ByModel(ctx, month)
	must(err)
	if len(byModel) != 1 || byModel[0].ModelGroup != "sonnet" || byModel[0].Calls != 21 || byModel[0].CostUSD != "3.500000" {
		t.Fatalf("by model = %+v", byModel)
	}
	byDay, err := s.Usage().ByDay(ctx, month)
	must(err)
	if len(byDay) != 2 || byDay[0].Day.Day() != 1 || byDay[0].Calls != 17 || byDay[1].Day.Day() != 2 || byDay[1].Calls != 4 {
		t.Fatalf("by day = %+v", byDay)
	}
	one, err := s.Usage().ByDay(ctx, repo.UsageFilter{From: d(1), To: d(31), EmployeeID: work2.ID})
	must(err)
	if len(one) != 1 || one[0].Calls != 5 {
		t.Fatalf("one employee's days = %+v", one)
	}
	rows, err := s.Usage().Rows(ctx, month, 3)
	must(err)
	if len(rows) != 3 {
		t.Fatalf("limit ignored: %d rows", len(rows))
	}
	first, last, err := s.Usage().Days(ctx)
	must(err)
	if first.Day() != 1 || last.Day() != 2 {
		t.Fatalf("days = %v .. %v", first, last)
	}
}
