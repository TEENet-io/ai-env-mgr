package litellm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// DailyUsage is one key's calls on one model group on one day, summed by the
// gateway itself (LiteLLM's daily team spend table).
type DailyUsage struct {
	KeyAlias         string
	ModelGroup       string
	Calls, Failures  int
	SpendUSD         float64
	PromptTokens     int64
	CompletionTokens int64
}

// The parts of /team/daily/activity read here. A model group's
// api_key_breakdown is what splits a day by key and model at once.
type dailyActivity struct {
	Results []struct {
		Date      string `json:"date"`
		Breakdown struct {
			ModelGroups map[string]struct {
				Keys map[string]struct {
					Metrics  dailyMetrics `json:"metrics"`
					Metadata struct {
						KeyAlias string `json:"key_alias"`
					} `json:"metadata"`
				} `json:"api_key_breakdown"`
			} `json:"model_groups"`
		} `json:"breakdown"`
	} `json:"results"`
	Metadata struct {
		HasMore bool `json:"has_more"`
	} `json:"metadata"`
}

type dailyMetrics struct {
	Spend            float64 `json:"spend"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	APIRequests      int     `json:"api_requests"`
	FailedRequests   int     `json:"failed_requests"`
}

// usagePageLimit bounds the pages read for one day. A day is one result, so
// more than one page is already unusual; the bound only stops a gateway that
// keeps saying has_more from holding the worker forever.
const usagePageLimit = 20

// TeamDailyUsage is what the team's keys spent on each model group on day
// (UTC), from the gateway's own daily aggregation. On a shared gateway the
// team narrows it to this customer (tenant-admin enforces the same); on an
// unshared one every team's keys are counted. Keys outside any team are not
// in this table, which is why every key this console mints names its team.
func (c *Client) TeamDailyUsage(ctx context.Context, day time.Time) ([]DailyUsage, error) {
	date := day.UTC().Format("2006-01-02")
	sums := map[[2]string]*DailyUsage{}
	for page := 1; ; page++ {
		if page > usagePageLimit {
			return nil, fmt.Errorf("daily usage for %s: more than %d pages", date, usagePageLimit)
		}
		q := url.Values{"start_date": {date}, "end_date": {date}, "page": {strconv.Itoa(page)}, "page_size": {"100"}}
		if team := c.Team(); team != "" {
			q.Set("team_ids", team)
		}
		var out dailyActivity
		if err := c.do(ctx, http.MethodGet, "/team/daily/activity?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, result := range out.Results {
			if result.Date != date {
				continue
			}
			for group, model := range result.Breakdown.ModelGroups {
				for _, key := range model.Keys {
					k := [2]string{key.Metadata.KeyAlias, group}
					sum := sums[k]
					if sum == nil {
						sum = &DailyUsage{KeyAlias: key.Metadata.KeyAlias, ModelGroup: group}
						sums[k] = sum
					}
					sum.Calls += key.Metrics.APIRequests
					sum.Failures += key.Metrics.FailedRequests
					sum.SpendUSD += key.Metrics.Spend
					sum.PromptTokens += key.Metrics.PromptTokens
					sum.CompletionTokens += key.Metrics.CompletionTokens
				}
			}
		}
		if !out.Metadata.HasMore {
			break
		}
	}
	rows := make([]DailyUsage, 0, len(sums))
	for _, sum := range sums {
		rows = append(rows, *sum)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].KeyAlias != rows[j].KeyAlias {
			return rows[i].KeyAlias < rows[j].KeyAlias
		}
		return rows[i].ModelGroup < rows[j].ModelGroup
	})
	return rows, nil
}
