package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/slsclient"
)

type fakeUsage struct {
	days map[string][]repo.UsageRow
	fail map[string]error
	asks []string
}

func (f *fakeUsage) DailyUsage(_ context.Context, day time.Time) ([]repo.UsageRow, error) {
	key := day.Format("2006-01-02")
	f.asks = append(f.asks, key)
	if err, bad := f.fail[key]; bad {
		return nil, err
	}
	return f.days[key], nil
}

func TestUsageSnapshotFillsThreeDaysAndResolvesAliases(t *testing.T) {
	store, ctx := newWorkerStore(t)
	work1, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	if err != nil {
		t.Fatal(err)
	}
	work2, err := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work2"})
	if err != nil {
		t.Fatal(err)
	}
	// work1 has a grant under a new-style alias; work2 has none, only the
	// old emp-<user> shape in the logs.
	cred, _ := store.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: work1.ID, Epoch: 1, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	alias := KeyAlias("work1", work1.ID, 1)
	if _, err := store.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: work1.ID, Epoch: 1, ExternalUser: "emp-work1", KeyAlias: alias, CredentialID: cred.ID}); err != nil {
		t.Fatal(err)
	}
	src := &fakeUsage{days: map[string][]repo.UsageRow{
		"2026-09-20": {
			{KeyAlias: alias, ModelGroup: "sonnet", Calls: 4, CostUSD: "0.4"},
			{KeyAlias: "emp-work2-e3", ModelGroup: "sonnet", Calls: 2, CostUSD: "0.2"},
			{KeyAlias: "sk-master", ModelGroup: "opus", Calls: 1, CostUSD: "1"},
		},
		"2026-09-19": {{KeyAlias: alias, ModelGroup: "sonnet", Calls: 1, CostUSD: "0.1"}},
		"2026-09-18": {{KeyAlias: alias, ModelGroup: "opus", Calls: 1, CostUSD: "2"}},
	}}
	now := time.Date(2026, 9, 21, 0, 40, 0, 0, time.UTC)
	h := UsageSnapshot{Store: store, Source: src, Now: func() time.Time { return now }}
	res, err := h.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.Note, "wrote 3 day(s), 5 row(s)") {
		t.Fatalf("note = %q", res.Note)
	}
	if strings.Join(src.asks, ",") != "2026-09-20,2026-09-19,2026-09-18" {
		t.Fatalf("asked for %v", src.asks)
	}
	sept := repo.UsageFilter{From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	rows, err := store.Usage().Rows(ctx, sept, 0)
	if err != nil {
		t.Fatal(err)
	}
	byAlias := map[string]string{}
	for _, r := range rows {
		byAlias[r.KeyAlias] = r.EmployeeID
	}
	if byAlias[alias] != work1.ID || byAlias["emp-work2-e3"] != work2.ID || byAlias["sk-master"] != "" {
		t.Fatalf("aliases resolved as %v", byAlias)
	}

	// The next night sees yesterday again with a bigger number; the day is
	// replaced, not doubled.
	src.days["2026-09-20"] = []repo.UsageRow{{KeyAlias: alias, ModelGroup: "sonnet", Calls: 6, CostUSD: "0.6"}}
	if _, err := h.Run(ctx, repo.Task{}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	days, _ := store.Usage().ByDay(ctx, sept)
	if len(days) != 3 || days[2].Day.Day() != 20 || days[2].Calls != 6 {
		t.Fatalf("days after rewrite = %+v", days)
	}
}

func TestUsageSnapshotSkipsADayTheLogServiceCannotAnswer(t *testing.T) {
	store, ctx := newWorkerStore(t)
	src := &fakeUsage{
		days: map[string][]repo.UsageRow{"2026-09-20": {{KeyAlias: "emp-x", Calls: 1}}, "2026-09-18": {{KeyAlias: "emp-x", Calls: 1}}},
		fail: map[string]error{"2026-09-19": errors.New("timeout")},
	}
	now := time.Date(2026, 9, 21, 0, 40, 0, 0, time.UTC)
	h := UsageSnapshot{Store: store, Source: src, Now: func() time.Time { return now }}
	res, err := h.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("one bad day must not fail the run: %v", err)
	}
	if !strings.Contains(res.Note, "wrote 2 day(s)") || !strings.Contains(res.Note, "1 day(s) skipped: 2026-09-19: timeout") {
		t.Fatalf("note = %q", res.Note)
	}
	// Every day failing is a failure: there is something to retry.
	src.fail = map[string]error{"2026-09-20": errors.New("down"), "2026-09-19": errors.New("down"), "2026-09-18": errors.New("down")}
	if _, err := h.Run(ctx, repo.Task{}); err == nil {
		t.Fatal("a source that answered nothing must fail the task")
	}
}

func TestUsageRowsFromReadsTheAggregateColumns(t *testing.T) {
	rows := usageRowsFrom([]slsclient.Log{
		{"employee_id": "emp-a-e1", "model_group": "sonnet", "calls": "12", "failures": "1", "cost_usd": "0.123456", "unpriced": "0", "prompt_tokens": "1000", "completion_tokens": "200"},
		{"employee_id": "emp-b-e1", "model_group": "", "calls": "1", "cost_usd": "null", "prompt_tokens": "null"},
		{"employee_id": "", "calls": "5"},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Calls != 12 || rows[0].Failures != 1 || rows[0].CostUSD != "0.123456" || rows[0].PromptTokens != 1000 || rows[0].CompletionTokens != 200 {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[1].CostUSD != "0" || rows[1].PromptTokens != 0 || rows[1].ModelGroup != "" {
		t.Errorf("blank columns must read as zero: %+v", rows[1])
	}
}

func TestSnapshotIsHeldUntilTheDayHasSettled(t *testing.T) {
	store, ctx := newWorkerStore(t)
	now := time.Date(2026, 9, 21, 0, 5, 0, 0, time.UTC)
	if err := enqueuePeriodicAfter(ctx, store, TaskUsageSnapshot, now, EveryUsageSnapshot, AfterUsageSnapshot, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Tasks().Claim(ctx, "w", []string{TaskUsageSnapshot}, time.Minute); err == nil {
		t.Fatal("the snapshot ran before 00:30")
	}
	open, _ := store.Tasks().ListOpen(ctx, 10)
	if len(open) != 1 || !open[0].NextRunAt.Equal(now.Truncate(24*time.Hour).Add(30*time.Minute)) {
		t.Fatalf("open = %+v", open)
	}
}
