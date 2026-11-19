package litellm

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTeamDailyUsageSumsEachAliasAndModelGroup(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/team/daily/activity" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "unexpected", 400)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		if page == "1" {
			// Two keys under one alias (a key replaced that day) add up;
			// another day in the answer is not this day's.
			_, _ = w.Write([]byte(`{"results":[
				{"date":"2026-11-17","breakdown":{"model_groups":{
					"gpt-6":{"api_key_breakdown":{
						"k1":{"metrics":{"spend":1.5,"api_requests":3,"failed_requests":1,"prompt_tokens":10,"completion_tokens":2},"metadata":{"key_alias":"bippo-emp-ann-e1"}},
						"k2":{"metrics":{"spend":0.5,"api_requests":1,"prompt_tokens":5,"completion_tokens":1},"metadata":{"key_alias":"bippo-emp-ann-e1"}}}}}}},
				{"date":"2026-11-16","breakdown":{"model_groups":{"gpt-6":{"api_key_breakdown":{
						"k3":{"metrics":{"spend":9,"api_requests":9},"metadata":{"key_alias":"bippo-emp-ann-e1"}}}}}}}],
				"metadata":{"has_more":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"date":"2026-11-17","breakdown":{"model_groups":{
			"claude":{"api_key_breakdown":{"k4":{"metrics":{"spend":2,"api_requests":2},"metadata":{"key_alias":"bippo-emp-bob-e1"}}}}}}}],
			"metadata":{"has_more":false}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	c.HTTP = srv.Client()
	c.TeamID = "bippo"
	rows, err := c.TeamDailyUsage(t.Context(), time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 || queries[0] != "end_date=2026-11-17&page=1&page_size=100&start_date=2026-11-17&team_ids=bippo" {
		t.Fatalf("queries: %v", queries)
	}
	want := []DailyUsage{
		{KeyAlias: "bippo-emp-ann-e1", ModelGroup: "gpt-6", Calls: 4, Failures: 1, SpendUSD: 2, PromptTokens: 15, CompletionTokens: 3},
		{KeyAlias: "bippo-emp-bob-e1", ModelGroup: "claude", Calls: 2, SpendUSD: 2},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows: %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d: %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestTeamDailyUsageStopsAGatewayThatNeverEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"metadata":{"has_more":true}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	c.HTTP = srv.Client()
	if _, err := c.TeamDailyUsage(t.Context(), time.Now()); err == nil {
		t.Fatal("no error from a gateway that always has more")
	}
}
