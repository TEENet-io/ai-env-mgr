package litellm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEmployeeIDsCarryTheTeam(t *testing.T) {
	if got := EmployeeUserID("", "Peter"); got != "emp-peter" {
		t.Errorf("no team: %q", got)
	}
	if got := EmployeeUserID("acme", "Peter"); got != "acme-emp-peter" {
		t.Errorf("team: %q", got)
	}
	id := "1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"
	if got := EmployeeKeyAlias("", "peter", id, 3); got != "emp-peter-1a2b3c4d-e3" {
		t.Errorf("no team alias: %q", got)
	}
	if got := EmployeeKeyAlias("acme", "peter", id, 3); got != "acme-emp-peter-1a2b3c4d-e3" {
		t.Errorf("team alias: %q", got)
	}
}

func TestValidateTeamID(t *testing.T) {
	for _, ok := range []string{"", "acme", "teenet", "cust_01"} {
		if err := ValidateTeamID(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	// Hyphens are refused: "a-emp-b" would make "a-emp-b-emp-x" ambiguous
	// with team "a"'s employee "b-emp-x".
	for _, bad := range []string{"a", "Acme", "a-emp-b", "acme corp", strings.Repeat("x", 33)} {
		if err := ValidateTeamID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGenerateKeyPutsTheKeyInTheTeam(t *testing.T) {
	var body map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = nil
		_ = json.Unmarshal(raw, &body)
		_, _ = io.WriteString(w, `{"key":"sk-new","key_alias":"acme-emp-peter-e1"}`)
	})
	c.TeamID = "acme"
	if _, err := c.GenerateKey(context.Background(), "acme-emp-peter-e1", "acme-emp-peter", []string{"m"}, nil); err != nil {
		t.Fatal(err)
	}
	if body["team_id"] != "acme" {
		t.Errorf("team_id = %v", body["team_id"])
	}

	c.TeamID = ""
	if _, err := c.GenerateKey(context.Background(), "emp-peter-e1", "emp-peter", []string{"m"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, sent := body["team_id"]; sent {
		t.Errorf("team_id sent without a team: %v", body["team_id"])
	}
}

func TestListsAreNarrowedToTheTeamButAliasLookupIsNot(t *testing.T) {
	var queries []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/key/list":
			_, _ = io.WriteString(w, `{"keys":[{"key_alias":"emp-old-e1","token":"h"}],"total_pages":1}`)
		case "/user/list":
			_, _ = io.WriteString(w, `{"users":[],"total_pages":1}`)
		}
	})
	c.TeamID = "acme"
	ctx := context.Background()
	if _, err := c.ListKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListUsers(ctx); err != nil {
		t.Fatal(err)
	}
	// A token minted before the console had a team has none, and must still
	// be found: reporting it missing would leave it working unnoticed.
	if _, found, err := c.FindKeyByAlias(ctx, "emp-old-e1"); err != nil || !found {
		t.Fatalf("alias lookup: found=%v err=%v", found, err)
	}
	if len(queries) != 3 {
		t.Fatalf("queries: %v", queries)
	}
	if !strings.Contains(queries[0], "team_id=acme") {
		t.Errorf("key list not narrowed: %s", queries[0])
	}
	if !strings.Contains(queries[1], "team=acme") {
		t.Errorf("user list not narrowed: %s", queries[1])
	}
	if strings.Contains(queries[2], "team") {
		t.Errorf("alias lookup narrowed: %s", queries[2])
	}
}

func TestUpsertUserCreatesTheTeamAndJoinsIt(t *testing.T) {
	var calls []string
	var member map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/team/info", "/user/info":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"not found","code":"404"}}`)
		case "/team/member_add":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &member)
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	c.TeamID = "acme"
	spec := UserSpec{UserID: "acme-emp-peter", Quota: BudgetOnly(20)}
	if err := c.UpsertUser(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"/team/info", "/team/new", "/user/info", "/user/new", "/team/member_add"}
	if strings.Join(calls, " ") != strings.Join(want, " ") {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	m, _ := member["member"].(map[string]any)
	if member["team_id"] != "acme" || m["user_id"] != "acme-emp-peter" || m["role"] != "user" {
		t.Errorf("member_add body = %v", member)
	}
}

func TestUpsertUserToleratesAnExistingMember(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/info":
			_, _ = io.WriteString(w, `{"user_info":{"user_id":"acme-emp-peter"}}`)
		case "/team/member_add":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"User already in team. Member: user_id=acme-emp-peter","type":"team_member_already_in_team","code":"400"}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	c.TeamID = "acme"
	if err := c.UpsertUser(context.Background(), UserSpec{UserID: "acme-emp-peter", Quota: BudgetOnly(20)}); err != nil {
		t.Fatalf("an existing member must not fail the upsert: %v", err)
	}
}

func TestUpsertUserFailsWhenTheTeamCannotBeChecked(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/team/info" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		t.Errorf("went on to %s after the team check failed", r.URL.Path)
	})
	c.TeamID = "acme"
	if err := c.UpsertUser(context.Background(), UserSpec{UserID: "acme-emp-peter", Quota: BudgetOnly(20)}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestUpsertUserWithoutATeamTouchesNoTeam(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/team/") {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		if r.URL.Path == "/user/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	if err := c.UpsertUser(context.Background(), UserSpec{UserID: "emp-peter", Quota: BudgetOnly(20)}); err != nil {
		t.Fatal(err)
	}
}
