package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/slsclient"
)

// UsageSource answers "what did each token spend on each model on this day",
// already summed. The log service is the only implementation; the interface
// is what the tests stand in for.
type UsageSource interface {
	DailyUsage(ctx context.Context, day time.Time) ([]repo.UsageRow, error)
}

// EnqueueUsageSnapshot queues a snapshot now, outside the nightly bucket: the
// button on the usage page. Keyed on the second, so two clicks are two runs.
func EnqueueUsageSnapshot(ctx context.Context, store repo.Store, at time.Time) error {
	_, _, err := store.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           TaskUsageSnapshot,
		IdempotencyKey: TaskUsageSnapshot + ":manual:" + at.UTC().Format(time.RFC3339),
		MaxAttempts:    2,
	})
	return err
}

// UsageSnapshot copies the last few days of gateway usage from the log
// service into usage_daily.
//
// It rewrites each day whole rather than adding to it: the log service is
// queried again tomorrow for the same day, and a sum that grew on every
// pass would bill everybody three times. Going back Backfill days is what
// covers a night the log service was not answering, and events that arrived
// late.
type UsageSnapshot struct {
	Store    repo.Store
	Source   UsageSource
	Backfill int // days to rewrite, counting back from yesterday; default 3
	Now      func() time.Time
}

// Run rewrites yesterday and the days before it. A day the source cannot
// answer is skipped and named in the note; only when no day at all could be
// read is the run a failure, because then there is something to retry.
func (h UsageSnapshot) Run(ctx context.Context, _ repo.Task) (Result, error) {
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	backfill := h.Backfill
	if backfill <= 0 {
		backfill = 3
	}
	today := now().UTC().Truncate(24 * time.Hour)
	written, rows := 0, 0
	var skipped []string
	var lastErr error
	for i := 1; i <= backfill; i++ {
		day := today.AddDate(0, 0, -i)
		usage, err := h.Source.DailyUsage(ctx, day)
		if err != nil {
			skipped = append(skipped, day.Format("2006-01-02")+": "+err.Error())
			lastErr = err
			continue
		}
		resolved, err := h.resolve(ctx, usage)
		if err != nil {
			return Result{}, err
		}
		err = h.Store.InTx(ctx, func(tx repo.Store) error {
			return tx.Usage().UpsertDay(ctx, day, resolved)
		})
		if err != nil {
			return Result{}, err
		}
		written++
		rows += len(resolved)
	}
	if written == 0 && lastErr != nil {
		return Result{}, ClassError("usage_source", fmt.Errorf("no day could be read: %w", lastErr))
	}
	note := fmt.Sprintf("wrote %d day(s), %d row(s)", written, rows)
	if len(skipped) > 0 {
		note += fmt.Sprintf("; %d day(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))
	}
	return Result{Note: note}, nil
}

// resolve names the employee behind each alias. A grant is the authority
// (its alias is exactly what the gateway logs); an alias of the older
// emp-<user> shape with no grant falls back to the account of that name, so
// usage from before grants were recorded still lands on somebody. Anything
// else is kept under its alias with no employee.
func (h UsageSnapshot) resolve(ctx context.Context, rows []repo.UsageRow) ([]repo.UsageRow, error) {
	cache := map[string]string{}
	out := make([]repo.UsageRow, 0, len(rows))
	for _, row := range rows {
		id, seen := cache[row.KeyAlias]
		if !seen {
			var err error
			id, err = h.employeeFor(ctx, row.KeyAlias)
			if err != nil {
				return nil, err
			}
			cache[row.KeyAlias] = id
		}
		row.EmployeeID = id
		out = append(out, row)
	}
	return out, nil
}

func (h UsageSnapshot) employeeFor(ctx context.Context, alias string) (string, error) {
	if alias == "" {
		return "", nil
	}
	grant, err := h.Store.Grants().ByKeyAlias(ctx, "", alias)
	if err == nil {
		return grant.EmployeeID, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return "", err
	}
	user, ok := strings.CutPrefix(alias, "emp-")
	if !ok || user == "" {
		return "", nil
	}
	// emp-<user>-e<n> and emp-<user>-<id8>-e<n> both start with the user;
	// the bare emp-<user> is the oldest shape. Try the longest first.
	if i := strings.Index(user, "-"); i > 0 {
		user = user[:i]
	}
	employee, err := h.Store.Employees().ByWindowsUser(ctx, user)
	if err == nil {
		return employee.ID, nil
	}
	if errors.Is(err, repo.ErrNotFound) {
		return "", nil
	}
	return "", err
}

// SLSUsage reads the gateway's llm_call events from the log service.
type SLSUsage struct {
	Client   *slsclient.Client
	Logstore string
	Timeout  time.Duration // one query; default 60s
}

// usageQuery sums one day per alias and model group. The inner distinct is
// what keeps the totals honest: one call can land in the logstore twice
// (Logtail re-reads a rotated file; the gateway retries a write), and the
// event_id is the same both times.
const usageQuery = `event_type: llm_call | select employee_id, model_group,` +
	` count(*) as calls, count_if(status='failure') as failures,` +
	` round(sum(try_cast(cost_usd as double)),6) as cost_usd,` +
	` count_if(cost_state='unknown') as unpriced,` +
	` sum(try_cast(prompt_tokens as bigint)) as prompt_tokens,` +
	` sum(try_cast(completion_tokens as bigint)) as completion_tokens` +
	` from (select distinct event_id, employee_id, model_group, status, cost_usd, cost_state, prompt_tokens, completion_tokens from log)` +
	` group by employee_id, model_group limit 10000`

// DailyUsage sums the UTC day. The window is the log service's own receive
// time, which trails the call by seconds normally and by hours after a
// backlog; rewriting three days each night absorbs the drift.
func (s SLSUsage) DailyUsage(ctx context.Context, day time.Time) ([]repo.UsageRow, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	from := day.UTC().Truncate(24 * time.Hour)
	to := from.Add(24 * time.Hour)
	res, err := s.Client.GetLogs(qctx, s.Logstore, usageQuery, from.Unix(), to.Unix()-1, 10000, 0, false)
	if err != nil {
		return nil, err
	}
	if !res.Complete {
		return nil, errors.New("the log service answered from an incomplete index")
	}
	return usageRowsFrom(res.Logs), nil
}

// usageRowsFrom turns the query's string columns into rows. A blank numeric
// column (no priced call that day, say) reads as zero.
func usageRowsFrom(logs []slsclient.Log) []repo.UsageRow {
	rows := make([]repo.UsageRow, 0, len(logs))
	for _, l := range logs {
		// The service renders a missing column as the string "null". A call
		// with no alias (a probe, the gateway's own default user) still
		// happened; it is kept under a placeholder so the totals add up.
		alias := strings.TrimSpace(l["employee_id"])
		if alias == "" || alias == "null" {
			alias = "-"
		}
		cost := strings.TrimSpace(l["cost_usd"])
		if cost == "" || cost == "null" {
			cost = "0"
		}
		model := strings.TrimSpace(l["model_group"])
		if model == "null" {
			model = ""
		}
		rows = append(rows, repo.UsageRow{
			KeyAlias:         alias,
			ModelGroup:       model,
			Calls:            atoi(l["calls"]),
			Failures:         atoi(l["failures"]),
			CostUSD:          cost,
			Unpriced:         atoi(l["unpriced"]),
			PromptTokens:     int64(atoi(l["prompt_tokens"])),
			CompletionTokens: int64(atoi(l["completion_tokens"])),
		})
	}
	return rows
}

func atoi(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return 0
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(n)
}
