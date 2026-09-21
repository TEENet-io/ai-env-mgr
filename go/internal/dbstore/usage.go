package dbstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type usageRepo struct{ q querier }

const usageColumns = `day, coalesce(employee_id::text, ''), key_alias, model_group, calls, failures,
	cost_usd::text, unpriced, prompt_tokens, completion_tokens`

func scanUsage(row scanner) (repo.UsageRow, error) {
	var u repo.UsageRow
	err := row.Scan(&u.Day, &u.EmployeeID, &u.KeyAlias, &u.ModelGroup, &u.Calls, &u.Failures,
		&u.CostUSD, &u.Unpriced, &u.PromptTokens, &u.CompletionTokens)
	return u, err
}

func (r usageRepo) UpsertDay(ctx context.Context, day time.Time, rows []repo.UsageRow) error {
	d := dateOf(day)
	if _, err := r.q.Exec(ctx, `delete from usage_daily where day = $1`, d); err != nil {
		return mapError(err, "replace usage day")
	}
	for _, u := range rows {
		var employee *string
		if u.EmployeeID != "" {
			id := u.EmployeeID
			employee = &id
		}
		cost := u.CostUSD
		if cost == "" {
			cost = "0"
		}
		_, err := r.q.Exec(ctx,
			`insert into usage_daily
			   (day, employee_id, key_alias, model_group, calls, failures, cost_usd, unpriced, prompt_tokens, completion_tokens)
			 values ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10)
			 on conflict (day, key_alias, model_group) do update set
			   employee_id = excluded.employee_id, calls = usage_daily.calls + excluded.calls,
			   failures = usage_daily.failures + excluded.failures, cost_usd = usage_daily.cost_usd + excluded.cost_usd,
			   unpriced = usage_daily.unpriced + excluded.unpriced,
			   prompt_tokens = usage_daily.prompt_tokens + excluded.prompt_tokens,
			   completion_tokens = usage_daily.completion_tokens + excluded.completion_tokens,
			   updated_at = now()`,
			d, employee, u.KeyAlias, u.ModelGroup, u.Calls, u.Failures, cost, u.Unpriced, u.PromptTokens, u.CompletionTokens)
		if err != nil {
			return mapError(err, "write usage row")
		}
	}
	return nil
}

// The three summaries differ only in what they group by; the sums and the
// filter are the same, so they share one query body.
const usageSums = `sum(calls), sum(failures), sum(cost_usd)::text, sum(unpriced), sum(prompt_tokens), sum(completion_tokens)
	from usage_daily
	where day >= $1 and day < $2 and ($3 = '' or employee_id::text = $3)`

func (r usageRepo) ByEmployee(ctx context.Context, f repo.UsageFilter) ([]repo.UsageRow, error) {
	return r.query(ctx, "sum usage by employee",
		`select '0001-01-01'::date, coalesce(employee_id::text, ''), min(key_alias), '', `+usageSums+`
		 group by employee_id order by sum(cost_usd) desc, min(key_alias)`, f)
}

func (r usageRepo) ByModel(ctx context.Context, f repo.UsageFilter) ([]repo.UsageRow, error) {
	return r.query(ctx, "sum usage by model",
		`select '0001-01-01'::date, '', '', model_group, `+usageSums+`
		 group by model_group order by sum(cost_usd) desc, model_group`, f)
}

func (r usageRepo) ByDay(ctx context.Context, f repo.UsageFilter) ([]repo.UsageRow, error) {
	return r.query(ctx, "sum usage by day",
		`select day, '', '', '', `+usageSums+`
		 group by day order by day`, f)
}

func (r usageRepo) Rows(ctx context.Context, f repo.UsageFilter, limit int) ([]repo.UsageRow, error) {
	if limit <= 0 {
		limit = 50000
	}
	rows, err := r.q.Query(ctx,
		`select `+usageColumns+` from usage_daily
		  where day >= $1 and day < $2 and ($3 = '' or employee_id::text = $3)
		  order by day, key_alias, model_group limit $4`,
		dateOf(f.From), dateOf(f.To), f.EmployeeID, limit)
	if err != nil {
		return nil, mapError(err, "list usage")
	}
	return collectUsage(rows, "list usage")
}

func (r usageRepo) Days(ctx context.Context) (time.Time, time.Time, error) {
	var first, last *time.Time
	err := r.q.QueryRow(ctx, `select min(day), max(day) from usage_daily`).Scan(&first, &last)
	if err != nil {
		return time.Time{}, time.Time{}, mapError(err, "usage range")
	}
	if first == nil || last == nil {
		return time.Time{}, time.Time{}, mapError(errNoRow, "usage range")
	}
	return *first, *last, nil
}

func (r usageRepo) query(ctx context.Context, what, sql string, f repo.UsageFilter) ([]repo.UsageRow, error) {
	rows, err := r.q.Query(ctx, sql, dateOf(f.From), dateOf(f.To), f.EmployeeID)
	if err != nil {
		return nil, mapError(err, what)
	}
	return collectUsage(rows, what)
}

func collectUsage(rows pgx.Rows, what string) ([]repo.UsageRow, error) {
	defer rows.Close()
	out := []repo.UsageRow{}
	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, mapError(err, what)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// dateOf drops the clock: the table is keyed by calendar day in UTC, and a
// timestamp with a time zone would otherwise land on the day before or
// after depending on where the caller was.
func dateOf(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
