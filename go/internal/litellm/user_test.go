package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUpsertUserCreatesWhenMissing(t *testing.T) {
	var created map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user/info":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"User emp-alice not found","code":"404"}}`)
		case r.URL.Path == "/user/new":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &created)
			_, _ = io.WriteString(w, `{"user_id":"emp-alice"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	spec := UserSpec{UserID: "emp-alice", Alias: "Alice", Department: "研发",
		Quota: Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4}, Models: []string{"glm-5.2"}}
	if err := c.UpsertUser(context.Background(), spec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	want := map[string]any{
		"user_id": "emp-alice", "user_alias": "Alice", "user_role": "internal_user",
		"max_budget": float64(20), "budget_duration": "1mo",
		"rpm_limit": float64(60), "tpm_limit": float64(200000), "max_parallel_requests": float64(4),
		"auto_create_key": false,
	}
	for k, v := range want {
		if created[k] != v {
			t.Errorf("%s = %v, want %v", k, created[k], v)
		}
	}
	if md, _ := created["metadata"].(map[string]any); md["department"] != "研发" {
		t.Errorf("department not sent: %v", created["metadata"])
	}
	if models, _ := created["models"].([]any); len(models) != 1 || models[0] != "glm-5.2" {
		t.Errorf("models not sent: %v", created["models"])
	}
}

func TestUpsertUserUpdatesWhenPresent(t *testing.T) {
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/info":
			_, _ = io.WriteString(w, `{"user_id":"emp-alice","user_info":{"user_id":"emp-alice","spend":1.2}}`)
		case "/user/update":
			path = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"max_budget":30`) {
				t.Errorf("update body missing budget: %s", body)
			}
			if strings.Contains(string(body), "auto_create_key") || strings.Contains(string(body), "user_role") {
				t.Errorf("update must not resend creation-only fields: %s", body)
			}
			_, _ = io.WriteString(w, `{"user_id":"emp-alice","data":{}}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	err := c.UpsertUser(context.Background(), UserSpec{UserID: "emp-alice", Quota: Quota{MonthlyBudgetUSD: 30, RPM: 1, TPM: 1, Parallel: 1}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if path != "/user/update" {
		t.Fatal("existing user must be updated, not re-created")
	}
}

func TestUpsertUserRejectsInvalidQuota(t *testing.T) {
	// /user/update ignores null, so a zero cannot mean "unlimited" later; the
	// client refuses it up front instead of silently keeping the old value.
	called := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	err := c.UpsertUser(context.Background(), UserSpec{UserID: "emp-alice", Quota: Quota{MonthlyBudgetUSD: 0, RPM: 1, TPM: 1, Parallel: 1}})
	if err == nil || called {
		t.Fatalf("zero budget must be refused locally, err=%v called=%v", err, called)
	}
}

func TestUserInfoReportsNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"User emp-x not found","code":"404"}}`)
	})
	_, found, err := c.UserInfo(context.Background(), "emp-x")
	if err != nil || found {
		t.Fatalf("404 must read as not-found without error: found=%v err=%v", found, err)
	}
}

func TestUserInfoParsesLimitsAndSpend(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("user_id") != "emp-alice" {
			t.Errorf("user_id query missing: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"user_id":"emp-alice","user_info":{"user_id":"emp-alice","user_alias":"Alice","spend":3.25,
			"max_budget":20,"budget_duration":"1mo","budget_reset_at":"2026-10-01T00:00:00Z",
			"rpm_limit":60,"tpm_limit":200000,"max_parallel_requests":4,"models":["glm-5.2"],
			"metadata":{"department":"研发"}},"keys":[{"key_alias":"emp-alice","token":"abc"}]}`)
	})
	u, found, err := c.UserInfo(context.Background(), "emp-alice")
	if err != nil || !found {
		t.Fatalf("info: found=%v err=%v", found, err)
	}
	q := u.Quota()
	if q.MonthlyBudgetUSD != 20 || q.RPM != 60 || q.TPM != 200000 || q.Parallel != 4 {
		t.Errorf("quota not parsed: %+v", q)
	}
	if u.Spend != 3.25 || u.BudgetResetAt != "2026-10-01T00:00:00Z" || u.Department() != "研发" || u.Alias != "Alice" {
		t.Errorf("fields not parsed: %+v", u)
	}
}

func TestListUsersFollowsPagination(t *testing.T) {
	var pages []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/list" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		pages = append(pages, r.URL.Query().Get("page"))
		if r.URL.Query().Get("page_size") != "100" {
			t.Errorf("page_size = %q", r.URL.Query().Get("page_size"))
		}
		if r.URL.Query().Get("page") == "1" {
			var b strings.Builder
			b.WriteString(`{"total_pages":2,"users":[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"user_id":"emp-%03d"}`, i)
			}
			b.WriteString(`]}`)
			_, _ = io.WriteString(w, b.String())
			return
		}
		_, _ = io.WriteString(w, `{"total_pages":2,"users":[{"user_id":"emp-last"}]}`)
	})
	users, err := c.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(users) != 101 || users[100].UserID != "emp-last" || len(pages) != 2 {
		t.Fatalf("pagination wrong: %d users, pages %v", len(users), pages)
	}
}

func TestQuotaValidate(t *testing.T) {
	ok := Quota{MonthlyBudgetUSD: 1, RPM: 1, TPM: 1, Parallel: 1}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid quota rejected: %v", err)
	}
	for _, bad := range []Quota{
		{MonthlyBudgetUSD: 0, RPM: 1, TPM: 1, Parallel: 1},
		{MonthlyBudgetUSD: 1, RPM: 0, TPM: 1, Parallel: 1},
		{MonthlyBudgetUSD: 1, RPM: 1, TPM: 0, Parallel: 1},
		{MonthlyBudgetUSD: 1, RPM: 1, TPM: 1, Parallel: 0},
		{MonthlyBudgetUSD: -5, RPM: 1, TPM: 1, Parallel: 1},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v should be rejected", bad)
		}
	}
}
