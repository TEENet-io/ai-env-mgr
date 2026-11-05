package tenantadmin

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
)

// TestAgainstLiteLLM drives a real LiteLLM through tenant-admin with the
// console's own gateway client, for two companies, and checks neither can
// reach the other. Set TEST_LITELLM_URL and TEST_LITELLM_KEY (an
// administrator key) to run it; it creates teams, users and keys there.
func TestAgainstLiteLLM(t *testing.T) {
	base, adminKey := os.Getenv("TEST_LITELLM_URL"), os.Getenv("TEST_LITELLM_KEY")
	if base == "" || adminKey == "" {
		t.Skip("TEST_LITELLM_URL / TEST_LITELLM_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := time.Now().Format("150405")
	teamA, teamB := "ita"+suffix, "itb"+suffix

	// The host-side step: teams are made with the administrator key, then
	// a token is issued for each.
	var cfg Config
	tokens := map[string]string{}
	for _, team := range []string{teamA, teamB} {
		gw := litellm.New(base, adminKey)
		gw.TeamID = team
		if err := gw.EnsureTeam(ctx); err != nil {
			t.Fatal(err)
		}
		plain, digest, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.AddToken(team, digest); err != nil {
			t.Fatal(err)
		}
		tokens[team] = plain
	}
	proxy := httptest.NewServer(&Server{
		Upstream: HTTPUpstream{BaseURL: base, AdminKey: adminKey},
		Config:   func() Config { return cfg },
		Log:      log.New(io.Discard, "", 0),
	})
	defer proxy.Close()

	// A console for each company, as adminweb builds it.
	console := func(team string) *litellm.Client {
		gw := litellm.New(proxy.URL, tokens[team])
		who, err := gw.WhoAmI(ctx)
		if err != nil || who != team {
			t.Fatalf("whoami %q: %v", who, err)
		}
		gw.TeamID = who
		return gw
	}
	a, b := console(teamA), console(teamB)

	// Company A runs an employee through the console's whole lifecycle.
	userA := litellm.EmployeeUserID(teamA, "ann")
	aliasA := litellm.EmployeeKeyAlias(teamA, "ann", "1a2b3c4d-0000", 1)
	must(t, a.UpsertUser(ctx, litellm.UserSpec{UserID: userA, Alias: "Ann", Department: "ops", Quota: litellm.BudgetOnly(20)}))
	must(t, a.UpsertUser(ctx, litellm.UserSpec{UserID: userA, Alias: "Ann", Department: "ops", Quota: litellm.BudgetOnly(30)}))
	keyA, err := a.GenerateKey(ctx, aliasA, userA, []string{}, map[string]string{"windows_user": "ann", "epoch": "1"})
	must(t, err)
	if keyA.Key == "" {
		t.Fatal("no plaintext key returned")
	}
	found, ok, err := a.FindKeyByAlias(ctx, aliasA)
	must(t, err)
	if !ok {
		t.Fatal("A cannot find its own key")
	}
	must(t, a.UpdateKey(ctx, found.Handle(), []string{}))
	if _, ok, err := a.UserInfo(ctx, userA); err != nil || !ok {
		t.Fatalf("A user info: %v %v", ok, err)
	}
	keys, err := a.ListKeys(ctx)
	must(t, err)
	users, err := a.ListUsers(ctx)
	must(t, err)
	if len(keys) != 1 || len(users) != 1 {
		t.Fatalf("A sees %d keys and %d users, want 1 and 1", len(keys), len(users))
	}
	if _, err := a.Models(ctx); err != nil {
		t.Fatal(err)
	}

	// Company B sees none of it and can touch none of it.
	keys, err = b.ListKeys(ctx)
	must(t, err)
	users, err = b.ListUsers(ctx)
	must(t, err)
	if len(keys) != 0 || len(users) != 0 {
		t.Fatalf("B sees %d keys and %d users of A's", len(keys), len(users))
	}
	if _, ok, err := b.FindKeyByAlias(ctx, aliasA); err != nil || ok {
		t.Fatalf("B found A's key: %v %v", ok, err)
	}
	forbidden(t, "B reads A's user", func() error { _, _, err := b.UserInfo(ctx, userA); return err }())
	forbidden(t, "B updates A's user", b.UpsertUser(ctx, litellm.UserSpec{UserID: userA, Quota: litellm.BudgetOnly(1)}))
	forbidden(t, "B changes A's key", b.UpdateKey(ctx, found.Handle(), []string{"x"}))
	forbidden(t, "B revokes A's key", b.DeleteKey(ctx, found.Handle()))
	forbidden(t, "B revokes A's alias", b.DeleteKeyByAlias(ctx, aliasA))
	forbidden(t, "B deletes A's user", b.DeleteUser(ctx, userA))
	forbidden(t, "B mints a key on A's user", func() error {
		_, err := b.GenerateKey(ctx, litellm.EmployeeKeyAlias(teamB, "x", "00000000", 1), userA, nil, nil)
		return err
	}())

	// A's key still works for A after all that, then A offboards the employee.
	if _, ok, err := a.FindKeyByAlias(ctx, aliasA); err != nil || !ok {
		t.Fatalf("A's key after B's attempts: %v %v", ok, err)
	}
	must(t, a.DeleteKeyByAlias(ctx, aliasA))
	must(t, a.DeleteUser(ctx, userA))
	if _, ok, err := a.UserInfo(ctx, userA); err != nil || ok {
		t.Fatalf("user after delete: %v %v", ok, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func forbidden(t *testing.T, what string, err error) {
	t.Helper()
	var apiErr *litellm.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || !strings.Contains(apiErr.Body, "tenant_forbidden") {
		t.Errorf("%s: want 403 tenant_forbidden, got %v", what, err)
	}
}
