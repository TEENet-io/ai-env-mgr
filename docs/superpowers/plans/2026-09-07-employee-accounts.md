# Employee Accounts & Usage Limits Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One "员工账号" page in the windows-control console where an administrator opens an account (roster + gateway user + token + Codex config in one click), closes it (everything revoked in one click), and sets a monthly USD budget plus rate limits that the gateway enforces.

**Architecture:** Each employee becomes a LiteLLM *internal user* (`user_id = emp-<windowsUser lowercased>`, identical to the existing key alias) that carries budget, rate limits, model allowlist and spend; the employee's token hangs under that user and carries no limits of its own. The console roster (`admin/users.json`) keeps name, department and enabled state. `admincore` orchestrates five idempotent multi-step actions (onboard / offboard / quota / models / reissue) and appends one JSONL audit line per action; `adminweb` renders the roster joined with gateway state and flags every inconsistency with a button that re-runs the fixing action.

**Tech Stack:** Go 1.25, `net/http` + `html/template` (existing console), LiteLLM management API (`/user/*`, `/key/*`, `/model/info`), Alibaba OSS via the existing `Store` interface. No new dependencies.

**Spec:** `/root/pp_home/windows-pc/设计_员工账号管理与用量限制_2026-09.md`

## Global Constraints

- Repo: `/root/pp_home/windows-pc/ai-env-mgr/go` (module `github.com/TEENet-io/ai-env-mgr`). Run all `go` commands from there.
- Gateway user id and key alias are the same string, produced only by `admincore.KeyAlias(windowsUser)` = `"emp-" + strings.ToLower(windowsUser)`.
- Tokens carry **no** `max_budget`, `rpm_limit`, `tpm_limit`; limits live on the user only.
- `budget_duration` is always the literal `"1mo"`.
- Quota numbers are required and must be `> 0`. LiteLLM's `/user/update` ignores `null`, so "unlimited" cannot be expressed after creation; an administrator who wants no practical limit types a large number.
- Audit write failures are logged with `log.Printf` and never fail the action.
- Verified LiteLLM semantics (2026-09-07, production gateway): `GET /user/info?user_id=X` → 404 when unknown; `POST /user/new` → 409 when the id exists; `GET /user/list?page=N&page_size=100` returns `{users, total, page, page_size, total_pages}`; user-level budget is enforced on child keys with `429 budget_exceeded`.
- Commit messages: imperative, prefixed by package (`litellm:`, `admincore:`, `console:`), ending with:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  ```
- UI copy is Simplified Chinese, matching existing templates. Code comments are English.
- Do not modify anything under `cmd/agent/` or `internal/agentcore/`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/model/model.go` (modify) | `UserEntry` gains `Name`, `Department`. |
| `internal/litellm/client.go` (modify) | `Key.UserID`; `GenerateKey(ctx, alias, userID, models, metadata)` — budget parameter removed. |
| `internal/litellm/user.go` (create) | `User`, `Quota`, `UserSpec`; `UpsertUser`, `UserInfo`, `ListUsers`. |
| `internal/litellm/user_test.go` (create) | httptest coverage for the above. |
| `internal/admincore/codexgateway.go` (modify) | `Gateway` interface grows user methods; `ProvisionCodexGateway` ensures the user exists and mints the key under it; budget parameter removed. |
| `internal/admincore/quotadefaults.go` (create) | Load/save `admin/quota-defaults.json`; built-in defaults. |
| `internal/admincore/audit.go` (create) | `AuditEntry`; append and read `admin/audit/<user>.jsonl`. |
| `internal/admincore/account.go` (create) | `AccountSpec`; `Onboard`, `Offboard`, `SetQuota`, `Reissue`; `SetCodexGatewayModels` gains user update + audit. |
| `internal/admincore/*_test.go` | `fakeGateway` grows a users map; new tests per action. |
| `internal/adminweb/accounts.go` (create) | `accountRow`, `reconcileAccounts`; `/users`, `/users/detail` handlers and the five POST actions. |
| `internal/adminweb/gateway.go` (modify) | Keeps `gateway()`, `handleGateway` (models only), `contextWindowLabel`; loses holders/reconcile/provision/revoke. |
| `internal/adminweb/handlers.go` (modify) | `pageData` new fields; old `handleUsers` removed (moves to accounts.go). |
| `internal/adminweb/actions.go` (modify) | `actionUserAdd`/`actionUserEnabled` removed. |
| `internal/adminweb/server.go` (modify) | Routes; template funcs `usagepct`, `usagesev`. |
| `internal/adminweb/assets/users.html` (rewrite), `user.html` (create), `gateway.html` (slim), `settings.html` (add defaults form), `_layout.html` (nav label) | Templates. |
| `internal/adminweb/assets/static/app.css` (modify) | `.usage` bar styles reusing `.w0`…`.w100` width classes. |
| `gateway-litellm/config.yaml` (on the gateway host and in `/root/pp_home/windows-pc/gateway-litellm/`) | Per-token prices for glm-5.2 and deepseek-v4-flash. |

---

### Task 1: Roster fields and key ownership in the client

**Files:**
- Modify: `internal/model/model.go:167-172`
- Modify: `internal/litellm/client.go:55-62` (Key), `:87-113` (GenerateKey)
- Modify: `internal/litellm/client_test.go` (call sites of `GenerateKey`)
- Modify: `internal/admincore/codexgateway.go:17-24` (interface), `:113` (call), `internal/admincore/codexgateway_test.go:47` (fake)

**Interfaces:**
- Produces: `model.UserEntry{WindowsUser, CodexAccount, ClaudeAccount, Name, Department string; Enabled bool}`
- Produces: `litellm.Key.UserID string`
- Produces: `func (c *Client) GenerateKey(ctx context.Context, alias, userID string, models []string, metadata map[string]string) (Key, error)`

- [ ] **Step 1: Add the roster fields**

In `internal/model/model.go` replace the `UserEntry` struct:

```go
type UserEntry struct {
	WindowsUser   string `json:"windowsUser"`
	CodexAccount  string `json:"codexAccount"`
	ClaudeAccount string `json:"claudeAccount"`
	// Name and Department are labels for the administrator's benefit and
	// are mirrored onto the gateway user (user_alias, metadata.department)
	// so the gateway UI shows the same person. The roster is authoritative.
	Name       string `json:"name,omitempty"`
	Department string `json:"department,omitempty"`
	Enabled    bool   `json:"enabled"`
}
```

- [ ] **Step 2: Write the failing client test for user ownership**

Append to `internal/litellm/client_test.go`:

```go
func TestGenerateKeyAttachesKeyToUser(t *testing.T) {
	// Budget and rate limits live on the user; a key minted without user_id
	// would be enforced against nothing.
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"key":"sk-emp","key_alias":"emp-alice","user_id":"emp-alice"}`)
	})
	key, err := c.GenerateKey(context.Background(), "emp-alice", "emp-alice", []string{"glm-5.2"}, nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got["user_id"] != "emp-alice" {
		t.Errorf("user_id not sent: %v", got)
	}
	if _, has := got["max_budget"]; has {
		t.Errorf("key must not carry its own budget: %v", got)
	}
	if key.UserID != "emp-alice" {
		t.Errorf("UserID not parsed: %+v", key)
	}
}
```

Also update the three existing calls in `client_test.go` (`TestGenerateKeySendsAliasAndModels`, `TestGenerateKeyRejectsResponseWithoutToken`, `TestIsAliasTakenRecognizesDuplicate`, `TestIsAliasTakenIgnoresOtherFailures`) to the new signature, e.g.:

```go
key, err := c.GenerateKey(context.Background(), "emp-alice", "emp-alice", []string{"grok-4.6"}, map[string]string{"employee": "alice"})
```

and in `TestGenerateKeySendsAliasAndModels` replace the `max_budget` assertion with:

```go
	if got["user_id"] != "emp-alice" {
		t.Errorf("user_id = %v", got["user_id"])
	}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/litellm/ -run 'GenerateKey|IsAliasTaken' -v`
Expected: compile error, `too many arguments` / `undefined: Key.UserID`.

- [ ] **Step 4: Change Key and GenerateKey**

In `internal/litellm/client.go` replace the `Key` struct and `GenerateKey`:

```go
type Key struct {
	Key      string   `json:"key,omitempty"`   // plaintext; /key/generate only
	Token    string   `json:"token,omitempty"` // sha-256 of the plaintext; what /key/list returns
	KeyAlias string   `json:"key_alias"`
	UserID   string   `json:"user_id,omitempty"` // the internal user whose limits this key inherits
	Models   []string `json:"models"`
	Spend    float64  `json:"spend,omitempty"`
}
```

```go
// GenerateKey mints a token for one employee, owned by userID.
//
// alias must be stable per employee: the gateway enforces global uniqueness
// on it, which is what stops a double-click from issuing two live tokens for
// the same person. The plaintext token is returned once and never again, so
// the caller must deliver it before discarding the response.
//
// The key carries no budget or rate limit of its own. Those live on the
// user (see UpsertUser) so that they survive re-issuing the token; a limit
// on the key would reset to zero spend every time the token was replaced.
func (c *Client) GenerateKey(ctx context.Context, alias, userID string, models []string, metadata map[string]string) (Key, error) {
	if userID == "" {
		return Key{}, fmt.Errorf("generate key %q: no owning user", alias)
	}
	body := map[string]any{
		"key_alias": alias,
		"user_id":   userID,
		"models":    models,
	}
	if len(metadata) > 0 {
		body["metadata"] = metadata
	}

	var out Key
	if err := c.do(ctx, http.MethodPost, "/key/generate", body, &out); err != nil {
		return Key{}, err
	}
	if out.Key == "" {
		return Key{}, fmt.Errorf("gateway accepted /key/generate for %q but returned no token", alias)
	}
	return out, nil
}
```

Remove `MaxBudget` from `Key` (nothing else reads it; check with `grep -rn MaxBudget internal/`).

- [ ] **Step 5: Fix the admincore compile (interface, call, fake)**

In `internal/admincore/codexgateway.go` change the interface line to:

```go
	GenerateKey(ctx context.Context, alias, userID string, models []string, metadata map[string]string) (litellm.Key, error)
```

and the call at the mint site to:

```go
	key, err := gw.GenerateKey(ctx, alias, alias, allowed, map[string]string{"employee": windowsUser})
```

Leave the `maxBudget` parameter of `ProvisionCodexGateway` in place for now (Task 3 removes it).

In `internal/admincore/codexgateway_test.go` change the fake:

```go
func (f *fakeGateway) GenerateKey(_ context.Context, alias, userID string, models []string, _ map[string]string) (litellm.Key, error) {
	defer f.lock()()
	if f.generateErr != nil {
		return litellm.Key{}, f.generateErr
	}
	if userID == "" {
		return litellm.Key{}, fmt.Errorf("fake gateway: key without user_id")
	}
	if _, taken := f.existing[alias]; taken {
		return litellm.Key{}, &litellm.APIError{Status: 400, Path: "/key/generate", Body: "Key with alias '" + alias + "' already exists."}
	}
	k := litellm.Key{Key: "sk-" + alias, KeyAlias: alias, UserID: userID, Models: models}
	f.generated = append(f.generated, k)
	f.existing[alias] = litellm.Key{Token: "hash-of-" + alias, KeyAlias: alias, UserID: userID, Models: models}
	return k, nil
}
```

- [ ] **Step 6: Run the whole tree**

Run: `go build ./... && go test ./internal/litellm/ ./internal/admincore/ ./internal/adminweb/ ./internal/model/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/model/model.go internal/litellm/client.go internal/litellm/client_test.go internal/admincore/codexgateway.go internal/admincore/codexgateway_test.go
git commit -m "litellm: mint employee keys under an owning user; roster gains name and department"
```

---

### Task 2: Internal-user client (`UpsertUser`, `UserInfo`, `ListUsers`)

**Files:**
- Create: `internal/litellm/user.go`
- Create: `internal/litellm/user_test.go`

**Interfaces:**
- Produces:
  ```go
  type Quota struct { MonthlyBudgetUSD float64; RPM, TPM, Parallel int }
  type UserSpec struct { UserID, Alias, Department string; Quota Quota; Models []string }
  type User struct { UserID, Alias string; Spend float64; MaxBudget *float64; BudgetDuration, BudgetResetAt string; RPMLimit, TPMLimit, MaxParallel *int; Models []string; Metadata map[string]string }
  func (u User) Department() string
  func (u User) Quota() Quota
  func (c *Client) UpsertUser(ctx context.Context, spec UserSpec) error
  func (c *Client) UserInfo(ctx context.Context, userID string) (User, bool, error)
  func (c *Client) ListUsers(ctx context.Context) ([]User, error)
  func (q Quota) Validate() error
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/litellm/user_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/litellm/ -run 'User|Quota' -v`
Expected: compile errors (`undefined: UserSpec` etc.).

- [ ] **Step 3: Implement `user.go`**

Create `internal/litellm/user.go`:

```go
package litellm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Quota is the set of limits one employee lives under.
//
// Every field is required and positive. LiteLLM's /user/update drops null
// fields rather than clearing them, so once a limit is set it cannot be
// unset -- only raised. Refusing zero here keeps "unlimited" from being
// silently stored as "whatever it was before".
type Quota struct {
	MonthlyBudgetUSD float64 // resets on the 1st of each month (budget_duration "1mo")
	RPM              int     // requests per minute
	TPM              int     // tokens per minute
	Parallel         int     // concurrent requests
}

// Validate reports the first field that is not a positive number.
func (q Quota) Validate() error {
	switch {
	case !(q.MonthlyBudgetUSD > 0):
		return fmt.Errorf("monthly budget must be greater than zero")
	case q.RPM <= 0:
		return fmt.Errorf("rpm limit must be greater than zero")
	case q.TPM <= 0:
		return fmt.Errorf("tpm limit must be greater than zero")
	case q.Parallel <= 0:
		return fmt.Errorf("parallel limit must be greater than zero")
	}
	return nil
}

// budgetDuration is the only reset period this console uses. It is a
// calendar month: LiteLLM sets budget_reset_at to the first of next month.
const budgetDuration = "1mo"

// UserSpec is what the console wants a gateway user to look like.
type UserSpec struct {
	UserID     string   // admincore.KeyAlias(windowsUser)
	Alias      string   // display name, shown in the gateway UI
	Department string   // stored as metadata.department
	Quota      Quota
	Models     []string // allowlist; nil means every model the gateway routes
}

// User is an internal user as the gateway reports it.
//
// Pointer fields distinguish "not set" from zero. Spend is cumulative for
// the current budget period and is what the console shows against the
// budget; it survives the key being re-issued because it is the user's,
// not the key's.
type User struct {
	UserID         string            `json:"user_id"`
	Alias          string            `json:"user_alias"`
	Spend          float64           `json:"spend"`
	MaxBudget      *float64          `json:"max_budget"`
	BudgetDuration string            `json:"budget_duration"`
	BudgetResetAt  string            `json:"budget_reset_at"`
	RPMLimit       *int              `json:"rpm_limit"`
	TPMLimit       *int              `json:"tpm_limit"`
	MaxParallel    *int              `json:"max_parallel_requests"`
	Models         []string          `json:"models"`
	Metadata       map[string]string `json:"metadata"`
}

// Department returns metadata.department, or "".
func (u User) Department() string { return u.Metadata["department"] }

// Quota returns the user's limits, with zero for anything unset.
func (u User) Quota() Quota {
	var q Quota
	if u.MaxBudget != nil {
		q.MonthlyBudgetUSD = *u.MaxBudget
	}
	if u.RPMLimit != nil {
		q.RPM = *u.RPMLimit
	}
	if u.TPMLimit != nil {
		q.TPM = *u.TPMLimit
	}
	if u.MaxParallel != nil {
		q.Parallel = *u.MaxParallel
	}
	return q
}

// UpsertUser creates the gateway user for spec, or brings an existing one
// in line with it.
//
// Creation and update are separate endpoints with different accepted fields
// (user_role and auto_create_key are creation-only), so this looks the user
// up first rather than trying one and falling back on the other's error text.
func (c *Client) UpsertUser(ctx context.Context, spec UserSpec) error {
	if spec.UserID == "" {
		return fmt.Errorf("upsert user: empty user id")
	}
	if err := spec.Quota.Validate(); err != nil {
		return fmt.Errorf("upsert user %q: %w", spec.UserID, err)
	}
	body := map[string]any{
		"user_id":               spec.UserID,
		"user_alias":            spec.Alias,
		"max_budget":            spec.Quota.MonthlyBudgetUSD,
		"budget_duration":       budgetDuration,
		"rpm_limit":             spec.Quota.RPM,
		"tpm_limit":             spec.Quota.TPM,
		"max_parallel_requests": spec.Quota.Parallel,
		"metadata":              map[string]string{"department": spec.Department},
	}
	if spec.Models != nil {
		body["models"] = spec.Models
	}

	_, found, err := c.UserInfo(ctx, spec.UserID)
	if err != nil {
		return fmt.Errorf("upsert user %q: %w", spec.UserID, err)
	}
	if found {
		return c.do(ctx, http.MethodPost, "/user/update", body, nil)
	}
	body["user_role"] = "internal_user"
	// The console mints keys itself, under its own alias scheme.
	body["auto_create_key"] = false
	return c.do(ctx, http.MethodPost, "/user/new", body, nil)
}

// UserInfo fetches one user. An unknown id is (User{}, false, nil): the
// gateway answers 404, which for this call is an answer, not a failure.
func (c *Client) UserInfo(ctx context.Context, userID string) (User, bool, error) {
	if userID == "" {
		return User{}, false, fmt.Errorf("user info: empty user id")
	}
	var out struct {
		UserInfo *User `json:"user_info"`
	}
	err := c.do(ctx, http.MethodGet, "/user/info?user_id="+url.QueryEscape(userID), nil, &out)
	if err != nil {
		var apiErr *APIError
		if asAPIError(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return User{}, false, nil
		}
		return User{}, false, err
	}
	if out.UserInfo == nil {
		return User{}, false, nil
	}
	return *out.UserInfo, true, nil
}

// userPageSize mirrors keyPageSize; the gateway caps list pages at 100.
const userPageSize = 100

// ListUsers returns every internal user, following pagination to the end.
// Reconciliation joins this against the roster, so a truncated list would
// make a live account look like it was never opened.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var all []User
	for page := 1; page <= keyPageLimit; page++ {
		var out struct {
			Users      []User `json:"users"`
			TotalPages int    `json:"total_pages"`
		}
		path := fmt.Sprintf("/user/list?page=%d&page_size=%d", page, userPageSize)
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Users...)
		if page >= out.TotalPages || len(out.Users) < userPageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("gateway /user/list did not terminate after %d pages", keyPageLimit)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/litellm/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/litellm/user.go internal/litellm/user_test.go
git commit -m "litellm: manage internal users carrying budget and rate limits"
```

---

### Task 3: Gateway interface grows users; provisioning mints under the user

**Files:**
- Modify: `internal/admincore/codexgateway.go` (interface, `ProvisionCodexGateway`, `SetCodexGatewayModels`)
- Modify: `internal/admincore/codexgateway_test.go` (fake, existing tests)
- Modify: `internal/adminweb/gateway.go:179` (call site, temporary)

**Interfaces:**
- Produces:
  ```go
  type Gateway interface {
      GenerateKey(ctx, alias, userID string, models []string, metadata map[string]string) (litellm.Key, error)
      UpdateKey(ctx, key string, models []string) error
      DeleteKey(ctx, handles ...string) error
      DeleteKeyByAlias(ctx, alias string) error
      FindKeyByAlias(ctx, alias string) (litellm.Key, bool, error)
      ListKeys(ctx) ([]litellm.Key, error)
      Models(ctx) ([]litellm.Model, error)
      UpsertUser(ctx, spec litellm.UserSpec) error
      UserInfo(ctx, userID string) (litellm.User, bool, error)
      ListUsers(ctx) ([]litellm.User, error)
  }
  func (m *Manager) ProvisionCodexGateway(ctx, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) error
  func (m *Manager) ensureGatewayUser(ctx, gw Gateway, e model.UserEntry, quota *litellm.Quota, models []string) error
  ```
- `fakeGateway` gains `users map[string]litellm.User`, `upsertErr error`, `upserts []litellm.UserSpec`.

- [ ] **Step 1: Extend the fake and write the failing tests**

In `internal/admincore/codexgateway_test.go` extend `fakeGateway`:

```go
type fakeGateway struct {
	mu     *sync.Mutex // nil in single-threaded tests
	models []litellm.Model

	existing  map[string]litellm.Key
	generated []litellm.Key
	deleted   []string
	updated   map[string][]string

	users   map[string]litellm.User
	upserts []litellm.UserSpec

	generateErr error
	modelsErr   error
	upsertErr   error
	listKeysErr error
}
```

In `newFakeGateway` add `users: map[string]litellm.User{},`.

Add the methods:

```go
func (f *fakeGateway) ListKeys(context.Context) ([]litellm.Key, error) {
	defer f.lock()()
	if f.listKeysErr != nil {
		return nil, f.listKeysErr
	}
	out := make([]litellm.Key, 0, len(f.existing))
	for _, k := range f.existing {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeGateway) UpsertUser(_ context.Context, spec litellm.UserSpec) error {
	defer f.lock()()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	if err := spec.Quota.Validate(); err != nil {
		return err
	}
	f.upserts = append(f.upserts, spec)
	u := f.users[spec.UserID] // keep spend across updates, as the gateway does
	budget, rpm, tpm, par := spec.Quota.MonthlyBudgetUSD, spec.Quota.RPM, spec.Quota.TPM, spec.Quota.Parallel
	u.UserID, u.Alias = spec.UserID, spec.Alias
	u.MaxBudget, u.RPMLimit, u.TPMLimit, u.MaxParallel = &budget, &rpm, &tpm, &par
	u.BudgetDuration, u.BudgetResetAt = "1mo", "2026-10-01T00:00:00Z"
	u.Models = spec.Models
	u.Metadata = map[string]string{"department": spec.Department}
	f.users[spec.UserID] = u
	return nil
}

func (f *fakeGateway) UserInfo(_ context.Context, userID string) (litellm.User, bool, error) {
	defer f.lock()()
	u, ok := f.users[userID]
	return u, ok, nil
}

func (f *fakeGateway) ListUsers(context.Context) ([]litellm.User, error) {
	defer f.lock()()
	out := make([]litellm.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	return out, nil
}
```

Update every existing `ProvisionCodexGateway(...)` call in the test file to drop the trailing budget argument (`, 5)` → `)`, `, 0)` → `)`).

Append new tests:

```go
func TestProvisionCreatesGatewayUserWhenMissing(t *testing.T) {
	// A key needs an owner or the budget has nothing to hang on. Existing
	// employees (weipeng, 2026-09) have a key and no user; re-issuing must
	// heal that rather than fail.
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()

	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	u, ok := gw.users["emp-alice"]
	if !ok {
		t.Fatal("gateway user was not created")
	}
	if u.Quota() != DefaultQuota {
		t.Errorf("a user created on the fly gets the built-in defaults, got %+v", u.Quota())
	}
	if len(gw.generated) != 1 || gw.generated[0].UserID != "emp-alice" {
		t.Errorf("key not minted under the user: %+v", gw.generated)
	}
}

func TestProvisionKeepsExistingUserQuota(t *testing.T) {
	m, _ := managerWithUser(t, "alice")
	gw := newFakeGateway()
	budget := 55.0
	gw.users["emp-alice"] = litellm.User{UserID: "emp-alice", MaxBudget: &budget}

	if err := m.ProvisionCodexGateway(context.Background(), gw, GatewayConfig{BaseURL: "https://gw.example"}, "alice", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(gw.upserts) != 0 {
		t.Errorf("re-issuing a token must not rewrite the user's quota: %+v", gw.upserts)
	}
}
```

Also in `TestProvisionDeliversConfigAndCatalog` the config assertion `sk-emp-alice` still holds.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/admincore/ -run Provision -v`
Expected: compile error (`fakeGateway does not implement Gateway`, `undefined: DefaultQuota`).

- [ ] **Step 3: Implement**

In `internal/admincore/codexgateway.go`:

Replace the `Gateway` interface:

```go
type Gateway interface {
	GenerateKey(ctx context.Context, alias, userID string, models []string, metadata map[string]string) (litellm.Key, error)
	UpdateKey(ctx context.Context, key string, models []string) error
	DeleteKey(ctx context.Context, handles ...string) error
	DeleteKeyByAlias(ctx context.Context, alias string) error
	FindKeyByAlias(ctx context.Context, alias string) (litellm.Key, bool, error)
	ListKeys(ctx context.Context) ([]litellm.Key, error)
	Models(ctx context.Context) ([]litellm.Model, error)

	// Internal users carry the budget and rate limits a key inherits.
	UpsertUser(ctx context.Context, spec litellm.UserSpec) error
	UserInfo(ctx context.Context, userID string) (litellm.User, bool, error)
	ListUsers(ctx context.Context) ([]litellm.User, error)
}
```

Add after `KeyAlias`:

```go
// DefaultQuota is what an account gets when nothing else has been decided:
// the built-in fallback behind admin/quota-defaults.json (see
// quotadefaults.go) and the quota given to a user created on the fly while
// re-issuing a token for an employee who predates user records.
var DefaultQuota = litellm.Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4}

// ensureGatewayUser makes sure the gateway user for e exists.
//
// With quota nil an existing user is left exactly as is -- re-issuing a
// token must not silently reset a budget an administrator tuned -- and a
// missing one is created with the stored defaults. With quota set the user
// is created or updated to match: that is the onboarding and quota-change
// path.
func (m *Manager) ensureGatewayUser(ctx context.Context, gw Gateway, e model.UserEntry, quota *litellm.Quota, models []string) error {
	id := KeyAlias(e.WindowsUser)
	existing, found, err := gw.UserInfo(ctx, id)
	if err != nil {
		return fmt.Errorf("look up gateway user %q: %w", id, err)
	}
	if found && quota == nil {
		return nil
	}
	spec := litellm.UserSpec{UserID: id, Alias: e.Name, Department: e.Department, Models: models}
	switch {
	case quota != nil:
		spec.Quota = *quota
	default:
		q, err := m.LoadQuotaDefaults()
		if err != nil {
			return err
		}
		spec.Quota = q
	}
	if spec.Alias == "" {
		spec.Alias = e.WindowsUser
	}
	if found && models == nil {
		spec.Models = existing.Models
	}
	if err := gw.UpsertUser(ctx, spec); err != nil {
		return fmt.Errorf("write gateway user %q: %w", id, err)
	}
	return nil
}
```

Change `ProvisionCodexGateway`'s signature and body: remove `maxBudget float64`; after `allowed, err := resolveAllowlist(...)` insert

```go
	if err := m.ensureGatewayUser(ctx, gw, *us.Find(windowsUser), nil, nil); err != nil {
		return err
	}
```

and change the mint to `gw.GenerateKey(ctx, alias, alias, allowed, map[string]string{"employee": windowsUser})`. Update the doc comment's first paragraph to:

```go
// ProvisionCodexGateway issues a gateway token for one employee, under their
// gateway user, and delivers the Codex configuration that uses it. Budget
// and rate limits are the user's, so they are untouched here.
```

In `SetCodexGatewayModels`, after `gw.UpdateKey(...)` succeeds, also mirror the allowlist onto the user so both halves agree:

```go
	if _, found, err := gw.UserInfo(ctx, alias); err != nil {
		return fmt.Errorf("look up gateway user %q: %w", alias, err)
	} else if found {
		if err := m.ensureGatewayUser(ctx, gw, *us.Find(windowsUser), nil, allowed); err != nil {
			return err
		}
	}
```

(`SetCodexGatewayModels` must load the roster first: add `us, err := m.LoadUsers()` and a `us.Find(windowsUser) == nil` → error check at the top, mirroring `ProvisionCodexGateway`.) Note `ensureGatewayUser` with `quota == nil` and `found` returns early — so for this call pass the existing quota explicitly:

```go
	} else if found {
		q := existingUser.Quota()
		if err := m.ensureGatewayUser(ctx, gw, *us.Find(windowsUser), &q, allowed); err != nil {
			return err
		}
	}
```

(rename the blank to `existingUser`.)

Temporarily fix the adminweb call site `internal/adminweb/gateway.go:179` to drop the budget argument and delete the `budget` parsing block above it (Task 10 replaces this handler anyway). Add a stub `LoadQuotaDefaults` so this compiles — Task 4 writes the real one; to keep Task 3 self-contained, create `internal/admincore/quotadefaults.go` now with only:

```go
package admincore

import "github.com/TEENet-io/ai-env-mgr/internal/litellm"

// LoadQuotaDefaults returns the quota a new account starts with. Task 4
// backs this with admin/quota-defaults.json; until then it is the built-in.
func (m *Manager) LoadQuotaDefaults() (litellm.Quota, error) { return DefaultQuota, nil }
```

- [ ] **Step 4: Run tests**

Run: `go build ./... && go test ./internal/admincore/ ./internal/adminweb/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/admincore/codexgateway.go internal/admincore/codexgateway_test.go internal/admincore/quotadefaults.go internal/adminweb/gateway.go
git commit -m "admincore: provision tokens under a gateway user that owns the limits"
```

---

### Task 4: Quota defaults stored in OSS

**Files:**
- Modify: `internal/admincore/quotadefaults.go`
- Create: `internal/admincore/quotadefaults_test.go`

**Interfaces:**
- Produces: `func (m *Manager) LoadQuotaDefaults() (litellm.Quota, error)`, `func (m *Manager) SaveQuotaDefaults(q litellm.Quota) error`, `func QuotaDefaultsKey() string` (= `ossclient.AdminKey("quota-defaults.json")`).

- [ ] **Step 1: Write the failing tests**

```go
package admincore

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
)

func TestQuotaDefaultsFallBackToBuiltIn(t *testing.T) {
	m, _ := newManager()
	q, err := m.LoadQuotaDefaults()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if q != DefaultQuota {
		t.Errorf("empty store should yield the built-in defaults, got %+v", q)
	}
}

func TestQuotaDefaultsRoundTrip(t *testing.T) {
	m, _ := newManager()
	want := litellm.Quota{MonthlyBudgetUSD: 35, RPM: 90, TPM: 300000, Parallel: 6}
	if err := m.SaveQuotaDefaults(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := m.LoadQuotaDefaults()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestQuotaDefaultsRejectInvalid(t *testing.T) {
	m, store := newManager()
	if err := m.SaveQuotaDefaults(litellm.Quota{MonthlyBudgetUSD: 0, RPM: 1, TPM: 1, Parallel: 1}); err == nil {
		t.Fatal("zero budget must be refused")
	}
	if _, ok := store.objects[QuotaDefaultsKey()]; ok {
		t.Error("invalid defaults must not be written")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/admincore/ -run QuotaDefaults -v`
Expected: `undefined: (*Manager).SaveQuotaDefaults`, `undefined: QuotaDefaultsKey`.

- [ ] **Step 3: Implement**

Replace `internal/admincore/quotadefaults.go`:

```go
package admincore

import (
	"encoding/json"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// QuotaDefaultsKey is where the console keeps the quota it pre-fills for a
// new account. It lives under admin/ like the roster: employees' machines
// never need it.
func QuotaDefaultsKey() string { return ossclient.AdminKey("quota-defaults.json") }

// quotaDefaultsFile is the on-disk shape; field names are stable so the
// object stays readable by hand.
type quotaDefaultsFile struct {
	MonthlyBudgetUSD float64 `json:"monthlyBudgetUSD"`
	RPM              int     `json:"rpm"`
	TPM              int     `json:"tpm"`
	Parallel         int     `json:"parallel"`
}

// LoadQuotaDefaults returns the quota a new account starts with. A missing
// or unreadable object yields the built-in defaults: the first run has no
// file, and a corrupt one should not stop anyone being onboarded.
func (m *Manager) LoadQuotaDefaults() (litellm.Quota, error) {
	data, _, err := m.Store.Get(QuotaDefaultsKey())
	if err != nil {
		return DefaultQuota, nil
	}
	var f quotaDefaultsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return DefaultQuota, nil
	}
	q := litellm.Quota{MonthlyBudgetUSD: f.MonthlyBudgetUSD, RPM: f.RPM, TPM: f.TPM, Parallel: f.Parallel}
	if err := q.Validate(); err != nil {
		return DefaultQuota, nil
	}
	return q, nil
}

// SaveQuotaDefaults stores the quota future accounts will be pre-filled
// with. It does not touch existing accounts.
func (m *Manager) SaveQuotaDefaults(q litellm.Quota) error {
	if err := q.Validate(); err != nil {
		return fmt.Errorf("quota defaults: %w", err)
	}
	data, err := json.MarshalIndent(quotaDefaultsFile{
		MonthlyBudgetUSD: q.MonthlyBudgetUSD, RPM: q.RPM, TPM: q.TPM, Parallel: q.Parallel,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quota defaults: %w", err)
	}
	if err := m.Store.Put(QuotaDefaultsKey(), data); err != nil {
		return fmt.Errorf("save quota defaults: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/admincore/ -run QuotaDefaults -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/admincore/quotadefaults.go internal/admincore/quotadefaults_test.go
git commit -m "admincore: store the quota pre-filled for new accounts"
```

---

### Task 5: Audit log (append and read)

**Files:**
- Create: `internal/admincore/audit.go`
- Create: `internal/admincore/audit_test.go`

**Interfaces:**
- Produces:
  ```go
  type AuditAction string
  const (AuditOnboard AuditAction = "onboard"; AuditOffboard = "offboard"; AuditQuota = "quota"; AuditModels = "models"; AuditReissue = "reissue")
  type AuditEntry struct { At string `json:"at"`; Action AuditAction `json:"action"`; User string `json:"user"`; Detail map[string]any `json:"detail,omitempty"` }
  func AuditKey(windowsUser string) string
  func (m *Manager) appendAudit(windowsUser string, action AuditAction, detail map[string]any)   // never returns an error
  func (m *Manager) ReadAudit(windowsUser string) ([]AuditEntry, error)                           // newest first, ≤ auditReadLimit (200)
  ```

- [ ] **Step 1: Write the failing tests**

```go
package admincore

import (
	"strings"
	"testing"
)

func TestAuditAppendsOneLinePerAction(t *testing.T) {
	m, store := newManager()
	m.appendAudit("alice", AuditOnboard, map[string]any{"budget": 20})
	m.appendAudit("alice", AuditQuota, map[string]any{"budget": 30})

	raw := string(store.objects[AuditKey("alice")])
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two JSONL lines, got %q", raw)
	}
	if !strings.Contains(lines[0], `"action":"onboard"`) || !strings.Contains(lines[1], `"action":"quota"`) {
		t.Errorf("lines out of order or mislabelled:\n%s", raw)
	}
}

func TestAuditReadReturnsNewestFirst(t *testing.T) {
	m, _ := newManager()
	m.appendAudit("alice", AuditOnboard, nil)
	m.appendAudit("alice", AuditOffboard, nil)
	entries, err := m.ReadAudit("alice")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 2 || entries[0].Action != AuditOffboard || entries[1].Action != AuditOnboard {
		t.Errorf("expected newest first, got %+v", entries)
	}
	if entries[0].User != "alice" || entries[0].At == "" {
		t.Errorf("entry missing user or timestamp: %+v", entries[0])
	}
}

func TestAuditReadCapsAtLimit(t *testing.T) {
	m, _ := newManager()
	for i := 0; i < auditReadLimit+5; i++ {
		m.appendAudit("alice", AuditQuota, map[string]any{"i": i})
	}
	entries, err := m.ReadAudit("alice")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != auditReadLimit {
		t.Errorf("got %d entries, want %d", len(entries), auditReadLimit)
	}
	if entries[0].Detail["i"] != float64(auditReadLimit+4) {
		t.Errorf("newest entry should be first, got %+v", entries[0].Detail)
	}
}

func TestAuditMissingFileIsEmpty(t *testing.T) {
	m, _ := newManager()
	entries, err := m.ReadAudit("nobody")
	if err != nil || len(entries) != 0 {
		t.Fatalf("no file should read as no history: %v %v", entries, err)
	}
}

func TestAuditWriteFailureDoesNotPanic(t *testing.T) {
	m, store := newManager()
	store.putErr = errNotFound
	m.appendAudit("alice", AuditOnboard, nil) // must only log
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/admincore/ -run Audit -v`
Expected: `undefined: AuditOnboard` etc.

- [ ] **Step 3: Implement**

Create `internal/admincore/audit.go`:

```go
package admincore

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// AuditAction names one of the five account operations.
type AuditAction string

const (
	AuditOnboard  AuditAction = "onboard"
	AuditOffboard AuditAction = "offboard"
	AuditQuota    AuditAction = "quota"
	AuditModels   AuditAction = "models"
	AuditReissue  AuditAction = "reissue"
)

// AuditEntry is one line of an employee's history.
//
// There is no operator field: the console signs in with an OSS key, not a
// personal identity, so "who" is not knowable here. Recording a guess would
// be worse than recording nothing.
type AuditEntry struct {
	At     string         `json:"at"`
	Action AuditAction    `json:"action"`
	User   string         `json:"user"`
	Detail map[string]any `json:"detail,omitempty"`
}

// AuditKey is one employee's history: admin/audit/<user>.jsonl.
func AuditKey(windowsUser string) string {
	return ossclient.AdminKey("audit/" + strings.ToLower(windowsUser) + ".jsonl")
}

// auditReadLimit bounds what the detail page shows. Newest first, so the
// cut falls on the oldest entries.
const auditReadLimit = 200

// appendAudit records one action. It never fails the caller: the object
// store has no append, so this is read-modify-write, and losing one audit
// line is a better outcome than rolling back an onboarding that succeeded.
func (m *Manager) appendAudit(windowsUser string, action AuditAction, detail map[string]any) {
	entry := AuditEntry{
		At:     time.Now().UTC().Format(time.RFC3339),
		Action: action,
		User:   windowsUser,
		Detail: detail,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		log.Printf("admincore: audit %s for %q: encode: %v", action, windowsUser, err)
		return
	}
	key := AuditKey(windowsUser)
	existing, _, _ := m.Store.Get(key) // missing is fine: first entry
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.Write(line)
	buf.WriteByte('\n')
	if err := m.Store.Put(key, buf.Bytes()); err != nil {
		log.Printf("admincore: audit %s for %q: write: %v", action, windowsUser, err)
	}
}

// ReadAudit returns an employee's history, newest first, at most
// auditReadLimit entries. No file means no history, not an error.
func (m *Manager) ReadAudit(windowsUser string) ([]AuditEntry, error) {
	data, _, err := m.Store.Get(AuditKey(windowsUser))
	if err != nil {
		return nil, nil
	}
	var all []AuditEntry
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// One damaged line should not hide the rest of the history.
			log.Printf("admincore: audit for %q: skip unreadable line: %v", windowsUser, err)
			continue
		}
		all = append(all, e)
	}
	// Reverse in place, then cut.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if len(all) > auditReadLimit {
		all = all[:auditReadLimit]
	}
	return all, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/admincore/ -run Audit -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/admincore/audit.go internal/admincore/audit_test.go
git commit -m "admincore: append-only audit history per employee"
```

---

### Task 6: `Onboard`

**Files:**
- Create: `internal/admincore/account.go`
- Create: `internal/admincore/account_test.go`

**Interfaces:**
- Produces:
  ```go
  type AccountSpec struct { WindowsUser, Name, Department string; Quota litellm.Quota; Models []string }
  func (m *Manager) Onboard(ctx context.Context, gw Gateway, cfg GatewayConfig, spec AccountSpec) error
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/admincore/account_test.go`:

```go
package admincore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

var testGW = GatewayConfig{BaseURL: "https://gw.example"}

func aliceSpec() AccountSpec {
	return AccountSpec{
		WindowsUser: "Alice", Name: "Alice Wang", Department: "研发",
		Quota:  litellm.Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4},
		Models: []string{"glm-5"},
	}
}

func TestOnboardOpensEverythingInOneStep(t *testing.T) {
	m, store := newManager()
	gw := newFakeGateway()

	if err := m.Onboard(context.Background(), gw, testGW, aliceSpec()); err != nil {
		t.Fatalf("onboard: %v", err)
	}

	// Roster: present, enabled, labelled.
	us, _ := m.LoadUsers()
	e := us.Find("alice")
	if e == nil || !e.Enabled || e.Name != "Alice Wang" || e.Department != "研发" {
		t.Fatalf("roster entry wrong: %+v", e)
	}
	// Gateway user: quota and labels mirrored.
	u, ok := gw.users["emp-alice"]
	if !ok {
		t.Fatal("gateway user not created")
	}
	if u.Quota() != aliceSpec().Quota || u.Alias != "Alice Wang" || u.Department() != "研发" {
		t.Errorf("gateway user not mirrored: %+v", u)
	}
	if len(u.Models) != 1 || u.Models[0] != "glm-5" {
		t.Errorf("user allowlist wrong: %v", u.Models)
	}
	// Token: minted under the user, restricted to the allowlist.
	if len(gw.generated) != 1 || gw.generated[0].UserID != "emp-alice" || len(gw.generated[0].Models) != 1 {
		t.Errorf("token wrong: %+v", gw.generated)
	}
	// Delivery: config and catalog in the credentials archive.
	set := deliveredSet(t, store, "Alice")
	if !strings.Contains(string(set[model.PathCodexConfig]), "sk-emp-alice") {
		t.Errorf("config does not carry the new token")
	}
	if len(set[model.PathCodexModels]) == 0 {
		t.Errorf("catalog not delivered")
	}
	// Audit.
	entries, _ := m.ReadAudit("Alice")
	if len(entries) != 1 || entries[0].Action != AuditOnboard {
		t.Errorf("audit missing: %+v", entries)
	}
}

func TestOnboardIsIdempotent(t *testing.T) {
	m, _ := newManager()
	gw := newFakeGateway()
	if err := m.Onboard(context.Background(), gw, testGW, aliceSpec()); err != nil {
		t.Fatalf("first: %v", err)
	}
	spec := aliceSpec()
	spec.Quota.MonthlyBudgetUSD = 40
	if err := m.Onboard(context.Background(), gw, testGW, spec); err != nil {
		t.Fatalf("second: %v", err)
	}
	us, _ := m.LoadUsers()
	if len(us.Users) != 1 {
		t.Errorf("second onboarding duplicated the roster entry: %+v", us.Users)
	}
	if got := gw.users["emp-alice"].Quota().MonthlyBudgetUSD; got != 40 {
		t.Errorf("quota not updated on re-onboard: %v", got)
	}
	if len(gw.generated) != 2 || len(gw.deleted) != 1 {
		t.Errorf("re-onboard must revoke the old token and mint one new: generated=%d deleted=%v", len(gw.generated), gw.deleted)
	}
}

func TestOnboardReenablesDepartedEmployee(t *testing.T) {
	m, _ := newManager()
	gw := newFakeGateway()
	_ = m.SaveUsers(model.Users{Users: []model.UserEntry{{WindowsUser: "alice", Enabled: false}}})
	if err := m.Onboard(context.Background(), gw, testGW, aliceSpec()); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	us, _ := m.LoadUsers()
	if e := us.Find("alice"); e == nil || !e.Enabled {
		t.Errorf("re-onboarding must re-enable: %+v", e)
	}
}

func TestOnboardRejectsInvalidQuotaBeforeTouchingAnything(t *testing.T) {
	m, store := newManager()
	gw := newFakeGateway()
	spec := aliceSpec()
	spec.Quota.RPM = 0
	if err := m.Onboard(context.Background(), gw, testGW, spec); err == nil {
		t.Fatal("expected an error")
	}
	if len(store.objects) != 0 || len(gw.upserts) != 0 || len(gw.generated) != 0 {
		t.Error("an invalid spec must not write anywhere")
	}
}

func TestOnboardStopsVisiblyWhenUserWriteFails(t *testing.T) {
	// Roster written, no user, no token: the list page shows "在职无令牌"
	// and re-running onboard is the fix.
	m, _ := newManager()
	gw := newFakeGateway()
	gw.upsertErr = errors.New("gateway down")
	err := m.Onboard(context.Background(), gw, testGW, aliceSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	us, _ := m.LoadUsers()
	if e := us.Find("alice"); e == nil || !e.Enabled {
		t.Errorf("roster should already be written: %+v", e)
	}
	if len(gw.generated) != 0 {
		t.Error("no token must be minted without a user")
	}
	if entries, _ := m.ReadAudit("alice"); len(entries) != 0 {
		t.Error("a failed onboarding is not audited as done")
	}
}

func TestOnboardWithdrawsTokenWhenDeliveryFails(t *testing.T) {
	m, store := newManager()
	gw := newFakeGateway()
	store.putErr = nil
	// Fail only the credentials.zip write: let roster and audit through.
	store.putErrFor = credsKey("alice")
	err := m.Onboard(context.Background(), gw, testGW, aliceSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(gw.generated) != 1 || len(gw.deleted) != 1 {
		t.Errorf("token minted then not withdrawn: generated=%d deleted=%v", len(gw.generated), gw.deleted)
	}
}
```

`store.putErrFor` does not exist yet. Add to `fakeStore` in `internal/admincore/manager_test.go`:

```go
	putErrFor string // when set, Put fails only for this exact key
```

and in `Put`:

```go
func (f *fakeStore) Put(key string, data []byte) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.putErrFor != "" && key == f.putErrFor {
		return errNotFound
	}
	f.objects[key] = data
	return nil
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/admincore/ -run Onboard -v`
Expected: `undefined: AccountSpec`, `undefined: (*Manager).Onboard`.

- [ ] **Step 3: Implement**

Create `internal/admincore/account.go`:

```go
package admincore

import (
	"context"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// AccountSpec is everything an administrator decides when opening an
// account. Models nil means every model the gateway routes.
type AccountSpec struct {
	WindowsUser string
	Name        string
	Department  string
	Quota       litellm.Quota
	Models      []string
}

// Onboard opens an account: roster entry, gateway user with its limits, a
// token under that user, and the Codex configuration delivered to the
// employee's machine. It is idempotent -- running it again for the same
// person updates the labels and quota, revokes the previous token and
// issues a new one.
//
// Order matters and is chosen so that a failure at any step leaves a state
// the account list can name (see adminweb reconcileAccounts) and a re-run
// repairs: the roster first (cheap, local), then the user (the token needs
// an owner), then the token, then delivery. A delivery failure withdraws
// the token just minted so nothing live is left unreferenced.
func (m *Manager) Onboard(ctx context.Context, gw Gateway, cfg GatewayConfig, spec AccountSpec) error {
	if spec.WindowsUser == "" {
		return fmt.Errorf("onboard: a Windows user name is required")
	}
	if err := spec.Quota.Validate(); err != nil {
		return fmt.Errorf("onboard %q: %w", spec.WindowsUser, err)
	}
	if cfg.BaseURL == "" {
		return fmt.Errorf("gateway base URL is not configured")
	}
	defer lockProvision(spec.WindowsUser)()

	// 1. Roster.
	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	e := us.Find(spec.WindowsUser)
	if e == nil {
		us.Users = append(us.Users, model.UserEntry{WindowsUser: spec.WindowsUser})
		e = &us.Users[len(us.Users)-1]
	}
	e.Name, e.Department, e.Enabled = spec.Name, spec.Department, true
	if err := m.SaveUsers(us); err != nil {
		return err
	}
	entry := *e

	// 2. Gateway user, with the quota the administrator chose. The
	// allowlist is validated against the gateway first so a typo is
	// reported before anything is written there.
	available, err := gw.Models(ctx)
	if err != nil {
		return fmt.Errorf("read gateway model catalog: %w", err)
	}
	allowed, err := resolveAllowlist(available, spec.Models)
	if err != nil {
		return err
	}
	quota := spec.Quota
	if err := m.ensureGatewayUser(ctx, gw, entry, &quota, allowed); err != nil {
		return err
	}

	// 3 + 4. Token and delivery, shared with re-issuing.
	if err := m.ProvisionCodexGateway(ctx, gw, cfg, spec.WindowsUser, allowed); err != nil {
		return err
	}

	// 5. Audit.
	m.appendAudit(spec.WindowsUser, AuditOnboard, map[string]any{
		"name": spec.Name, "department": spec.Department,
		"budget": quota.MonthlyBudgetUSD, "rpm": quota.RPM, "tpm": quota.TPM, "parallel": quota.Parallel,
		"models": allowed,
	})
	return nil
}
```

`ProvisionCodexGateway` already takes `lockProvision`; since `Onboard` holds the same lock, make the lock re-entrant-free by splitting: rename the body of `ProvisionCodexGateway` into `func (m *Manager) provisionLocked(ctx, gw, cfg, windowsUser, models) error` (no lock), and have `ProvisionCodexGateway` be:

```go
func (m *Manager) ProvisionCodexGateway(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) error {
	defer lockProvision(windowsUser)()
	return m.provisionLocked(ctx, gw, cfg, windowsUser, models)
}
```

and `Onboard` calls `m.provisionLocked(...)` instead. Do the same split for `SetCodexGatewayModels` → `setModelsLocked` (Task 8 uses it).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/admincore/ -v`
Expected: PASS (all, including existing Provision tests).

- [ ] **Step 5: Commit**

```bash
git add internal/admincore/account.go internal/admincore/account_test.go internal/admincore/codexgateway.go internal/admincore/manager_test.go
git commit -m "admincore: onboard an employee in one idempotent step"
```

---

### Task 7: `Offboard`

**Files:**
- Modify: `internal/admincore/account.go`
- Modify: `internal/admincore/account_test.go`

**Interfaces:**
- Produces: `func (m *Manager) Offboard(ctx context.Context, gw Gateway, windowsUser string) error`
- Produces: `func (m *Manager) unbindUser(windowsUser string) (int, error)` — deletes every binding whose `User` equals `windowsUser` case-insensitively; returns how many.

- [ ] **Step 1: Write the failing tests**

Append to `account_test.go`:

```go
func onboarded(t *testing.T) (*Manager, *fakeStore, *fakeGateway) {
	t.Helper()
	m, store := newManager()
	gw := newFakeGateway()
	if err := m.Onboard(context.Background(), gw, testGW, aliceSpec()); err != nil {
		t.Fatalf("seed onboard: %v", err)
	}
	return m, store, gw
}

func TestOffboardRevokesEverythingAndKeepsHistory(t *testing.T) {
	m, store, gw := onboarded(t)
	if err := m.BindMachine("PC-1", "alice", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := m.BindMachine("PC-2", "Alice", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}

	if err := m.Offboard(context.Background(), gw, "alice"); err != nil {
		t.Fatalf("offboard: %v", err)
	}

	us, _ := m.LoadUsers()
	if e := us.Find("alice"); e == nil || e.Enabled {
		t.Errorf("roster must keep the entry, disabled: %+v", e)
	}
	if _, live := gw.existing["emp-alice"]; live {
		t.Error("token still live")
	}
	if _, ok := store.objects[credsKey("Alice")]; ok {
		t.Error("credentials.zip still in the store; the agent would never clear the machine")
	}
	bindings, _ := m.ListBindings()
	if len(bindings) != 0 {
		t.Errorf("machines still bound: %v", bindings)
	}
	if _, ok := gw.users["emp-alice"]; !ok {
		t.Error("gateway user must be kept for its spend history")
	}
	entries, _ := m.ReadAudit("alice")
	if len(entries) != 2 || entries[0].Action != AuditOffboard {
		t.Errorf("audit: %+v", entries)
	}
}

func TestOffboardContinuesPastGatewayFailure(t *testing.T) {
	// The gateway being down must not leave the files on the machine.
	m, store, gw := onboarded(t)
	_ = m.BindMachine("PC-1", "alice", "")
	gw.deleteAliasErr = errors.New("gateway down")

	err := m.Offboard(context.Background(), gw, "alice")
	if err == nil {
		t.Fatal("a failed revocation must be reported")
	}
	us, _ := m.LoadUsers()
	if e := us.Find("alice"); e.Enabled {
		t.Error("roster must be disabled even when the gateway fails")
	}
	if _, ok := store.objects[credsKey("Alice")]; ok {
		t.Error("credentials must be deleted even when the gateway fails")
	}
	if b, _ := m.ListBindings(); len(b) != 0 {
		t.Error("machines must be unbound even when the gateway fails")
	}
	if _, live := gw.existing["emp-alice"]; !live {
		t.Error("test setup: token should still be live so the list flags it")
	}
}

func TestOffboardUnknownUserIsAnError(t *testing.T) {
	m, _ := newManager()
	if err := m.Offboard(context.Background(), newFakeGateway(), "ghost"); err == nil {
		t.Fatal("offboarding someone not on the roster must be refused")
	}
}

func TestOffboardWithoutTokenSucceeds(t *testing.T) {
	m, _ := newManager()
	_ = m.SaveUsers(model.Users{Users: []model.UserEntry{{WindowsUser: "alice", Enabled: true}}})
	if err := m.Offboard(context.Background(), newFakeGateway(), "alice"); err != nil {
		t.Fatalf("no token is not a failure: %v", err)
	}
}
```

Add `deleteAliasErr error` to `fakeGateway` and honour it at the top of `DeleteKeyByAlias`:

```go
	if f.deleteAliasErr != nil {
		return f.deleteAliasErr
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/admincore/ -run Offboard -v`
Expected: `undefined: (*Manager).Offboard`.

- [ ] **Step 3: Implement**

Append to `account.go` (add `"errors"`, `"strings"` and `"github.com/TEENet-io/ai-env-mgr/internal/ossclient"` to imports):

```go
// Offboard closes an account: roster disabled, token revoked, delivered
// files withdrawn, machines unbound. The gateway user stays -- its spend
// history is the record of what this person used.
//
// Every step runs even if an earlier one failed. The two that matter for
// safety (disable, revoke) come first; the rest must still happen when the
// gateway is unreachable, or a departed employee's machine keeps a working
// configuration until someone remembers to try again. Failures are joined
// and reported, and the account list flags whatever is left over.
func (m *Manager) Offboard(ctx context.Context, gw Gateway, windowsUser string) error {
	defer lockProvision(windowsUser)()

	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	e := us.Find(windowsUser)
	if e == nil {
		return fmt.Errorf("user %q not found in roster", windowsUser)
	}
	// Use the roster's spelling from here on: stored objects are keyed by it.
	name := e.WindowsUser
	e.Enabled = false
	if err := m.SaveUsers(us); err != nil {
		return err
	}

	var failures []error
	alias := KeyAlias(name)
	if _, found, err := gw.FindKeyByAlias(ctx, alias); err != nil {
		failures = append(failures, fmt.Errorf("look up token: %w", err))
	} else if found {
		if err := gw.DeleteKeyByAlias(ctx, alias); err != nil {
			failures = append(failures, fmt.Errorf("revoke token: %w", err))
		}
	}
	if err := m.Store.Delete(credsKey(name)); err != nil {
		failures = append(failures, fmt.Errorf("withdraw delivered configuration: %w", err))
	}
	if _, err := m.unbindUser(name); err != nil {
		failures = append(failures, fmt.Errorf("unbind machines: %w", err))
	}

	if len(failures) > 0 {
		return fmt.Errorf("user %q is disabled, but: %w", name, errors.Join(failures...))
	}
	m.appendAudit(name, AuditOffboard, nil)
	return nil
}

// unbindUser removes every machine binding that points at windowsUser.
// Windows account names are case-insensitive, so the match is too.
func (m *Manager) unbindUser(windowsUser string) (int, error) {
	bindings, err := m.ListBindings()
	if err != nil {
		return 0, err
	}
	n := 0
	for machine, b := range bindings {
		if !strings.EqualFold(b.User, windowsUser) {
			continue
		}
		if err := m.Store.Delete(ossclient.BindingKey(machine)); err != nil {
			return n, fmt.Errorf("unbind %q: %w", machine, err)
		}
		n++
	}
	return n, nil
}
```

Note: on partial failure the audit line is deliberately **not** written (the action did not complete); the list page's flags carry the state instead.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/admincore/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/admincore/account.go internal/admincore/account_test.go internal/admincore/codexgateway_test.go
git commit -m "admincore: offboard revokes token, files and bindings in one step"
```

---

### Task 8: `SetQuota`, `SetModels` with audit, `Reissue`

**Files:**
- Modify: `internal/admincore/account.go`
- Modify: `internal/admincore/codexgateway.go` (`SetCodexGatewayModels` → thin wrapper)
- Modify: `internal/admincore/account_test.go`

**Interfaces:**
- Produces:
  ```go
  func (m *Manager) SetQuota(ctx context.Context, gw Gateway, windowsUser string, q litellm.Quota) error
  func (m *Manager) SetModels(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) error
  func (m *Manager) Reissue(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string) error
  ```
- `SetCodexGatewayModels` is removed; its only caller (`adminweb`) is rewritten in Task 10.

- [ ] **Step 1: Write the failing tests**

Append to `account_test.go`:

```go
func TestSetQuotaUpdatesUserOnly(t *testing.T) {
	m, _, gw := onboarded(t)
	q := litellm.Quota{MonthlyBudgetUSD: 50, RPM: 120, TPM: 400000, Parallel: 8}
	if err := m.SetQuota(context.Background(), gw, "alice", q); err != nil {
		t.Fatalf("quota: %v", err)
	}
	if gw.users["emp-alice"].Quota() != q {
		t.Errorf("quota not applied: %+v", gw.users["emp-alice"].Quota())
	}
	if len(gw.generated) != 1 {
		t.Error("changing a quota must not re-issue the token")
	}
	if len(gw.users["emp-alice"].Models) != 1 {
		t.Errorf("quota change must keep the allowlist: %v", gw.users["emp-alice"].Models)
	}
	if entries, _ := m.ReadAudit("alice"); entries[0].Action != AuditQuota || entries[0].Detail["budget"] != float64(50) {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestSetQuotaRequiresAnAccount(t *testing.T) {
	m, _ := newManager()
	_ = m.SaveUsers(model.Users{Users: []model.UserEntry{{WindowsUser: "alice", Enabled: true}}})
	err := m.SetQuota(context.Background(), newFakeGateway(), "alice", DefaultQuota)
	if err == nil {
		t.Fatal("no gateway user yet: the fix is onboarding, not a quota change")
	}
}

func TestSetModelsUpdatesUserTokenAndCatalog(t *testing.T) {
	m, store, gw := onboarded(t)
	if err := m.SetModels(context.Background(), gw, testGW, "alice", []string{"grok-4.6"}); err != nil {
		t.Fatalf("models: %v", err)
	}
	if got := gw.users["emp-alice"].Models; len(got) != 1 || got[0] != "grok-4.6" {
		t.Errorf("user allowlist: %v", got)
	}
	if got := gw.updated["hash-of-emp-alice"]; len(got) != 1 || got[0] != "grok-4.6" {
		t.Errorf("token allowlist: %v", gw.updated)
	}
	set := deliveredSet(t, store, "alice")
	if !strings.Contains(string(set[model.PathCodexModels]), `"slug": "grok-4.6"`) || strings.Contains(string(set[model.PathCodexModels]), `"slug": "glm-5"`) {
		t.Errorf("catalog not refreshed")
	}
	if entries, _ := m.ReadAudit("alice"); entries[0].Action != AuditModels {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestReissueMintsNewTokenAndAudits(t *testing.T) {
	m, _, gw := onboarded(t)
	if err := m.Reissue(context.Background(), gw, testGW, "alice"); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if len(gw.generated) != 2 || len(gw.deleted) != 1 {
		t.Errorf("reissue must revoke then mint: generated=%d deleted=%v", len(gw.generated), gw.deleted)
	}
	if entries, _ := m.ReadAudit("alice"); entries[0].Action != AuditReissue {
		t.Errorf("audit: %+v", entries[0])
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/admincore/ -run 'SetQuota|SetModels|Reissue' -v`
Expected: undefined methods.

- [ ] **Step 3: Implement**

Append to `account.go`:

```go
// SetQuota changes an employee's limits. It takes effect at the gateway
// immediately and touches nothing on the machine.
//
// It requires the gateway user to exist: an employee with a token but no
// user predates this feature, and the repair for that is Onboard, which
// creates the user with the right labels; quietly creating one here would
// leave it unlabelled.
func (m *Manager) SetQuota(ctx context.Context, gw Gateway, windowsUser string, q litellm.Quota) error {
	if err := q.Validate(); err != nil {
		return fmt.Errorf("quota for %q: %w", windowsUser, err)
	}
	defer lockProvision(windowsUser)()

	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	e := us.Find(windowsUser)
	if e == nil {
		return fmt.Errorf("user %q not found in roster", windowsUser)
	}
	id := KeyAlias(e.WindowsUser)
	existing, found, err := gw.UserInfo(ctx, id)
	if err != nil {
		return fmt.Errorf("look up gateway user %q: %w", id, err)
	}
	if !found {
		return fmt.Errorf("user %q has no gateway account yet; open one first", e.WindowsUser)
	}
	if err := m.ensureGatewayUser(ctx, gw, *e, &q, existing.Models); err != nil {
		return err
	}
	m.appendAudit(e.WindowsUser, AuditQuota, map[string]any{
		"budget": q.MonthlyBudgetUSD, "rpm": q.RPM, "tpm": q.TPM, "parallel": q.Parallel,
	})
	return nil
}

// SetModels changes which models an employee may use, on the user, on the
// token and in the delivered catalog. All three are needed: the user and
// token govern what they can reach, the catalog what they can see.
func (m *Manager) SetModels(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string, models []string) error {
	defer lockProvision(windowsUser)()
	allowed, err := m.setModelsLocked(ctx, gw, cfg, windowsUser, models)
	if err != nil {
		return err
	}
	m.appendAudit(windowsUser, AuditModels, map[string]any{"models": allowed})
	return nil
}

// Reissue replaces an employee's token and redelivers the configuration.
// The quota is the user's and is untouched.
func (m *Manager) Reissue(ctx context.Context, gw Gateway, cfg GatewayConfig, windowsUser string) error {
	defer lockProvision(windowsUser)()
	if err := m.provisionLocked(ctx, gw, cfg, windowsUser, nil); err != nil {
		return err
	}
	m.appendAudit(windowsUser, AuditReissue, nil)
	return nil
}
```

In `codexgateway.go`, turn `SetCodexGatewayModels` into `setModelsLocked` returning `([]string, error)` (the resolved allowlist), with no lock of its own, the roster lookup added in Task 3, and the user mirror. Delete the exported `SetCodexGatewayModels`. Keep `ProvisionCodexGateway` exported (Task 3 shape) — `Reissue` is the audited form the web uses.

Note on `Reissue` and the allowlist: `provisionLocked(..., nil)` resolves `nil` to "everything the gateway offers". That would widen a narrowed allowlist on re-issue. Fix inside `provisionLocked`: when `models == nil`, and the gateway user exists with a non-empty `Models`, use that as the request:

```go
	if models == nil {
		if u, found, err := gw.UserInfo(ctx, KeyAlias(windowsUser)); err != nil {
			return fmt.Errorf("look up gateway user: %w", err)
		} else if found && len(u.Models) > 0 {
			models = u.Models
		}
	}
```

Place this before `resolveAllowlist`. Add a test:

```go
func TestReissueKeepsNarrowedAllowlist(t *testing.T) {
	m, store, gw := onboarded(t) // alice: glm-5 only
	if err := m.Reissue(context.Background(), gw, testGW, "alice"); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if got := gw.generated[1].Models; len(got) != 1 || got[0] != "glm-5" {
		t.Errorf("reissue widened the allowlist: %v", got)
	}
	if strings.Contains(string(deliveredSet(t, store, "alice")[model.PathCodexModels]), `"slug": "grok-4.6"`) {
		t.Error("catalog widened on reissue")
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go build ./... 2>&1 | head` — expect only `internal/adminweb` failing on `SetCodexGatewayModels`/`RevokeCodexGateway`/`ProvisionCodexGateway` call sites; then `go test ./internal/admincore/ -v` → PASS. (adminweb is repaired in Task 10; if you need a green build now, temporarily point `actionGatewayProvision` at `Reissue` and delete the budget parsing.)

- [ ] **Step 5: Commit**

```bash
git add internal/admincore/account.go internal/admincore/account_test.go internal/admincore/codexgateway.go internal/adminweb/gateway.go
git commit -m "admincore: quota, model and reissue actions with audit"
```

---

### Task 9: Account reconciliation (pure function)

**Files:**
- Create: `internal/adminweb/accounts.go` (types + `reconcileAccounts` only, handlers come in Task 10)
- Create: `internal/adminweb/accounts_test.go`
- Delete from `internal/adminweb/gateway.go`: `gatewayHolder`, `reconcile`; delete the three `TestReconcile*` tests in `gateway_test.go` (they are superseded below).

**Interfaces:**
- Produces:
  ```go
  type accountRow struct {
      WindowsUser, Name, Department string
      Enabled       bool
      HasUser       bool          // gateway user exists
      HasToken      bool
      Models        []string      // from the user when present, else from the token
      Spend, Budget float64
      BudgetResetAt string
      Quota         litellm.Quota
      Machines      []string      // bound machines
      Flags         []accountFlag
  }
  type accountFlag struct { Label, Severity, Fix string }  // Severity: "warn"|"bad"; Fix: "onboard"|"offboard"
  func reconcileAccounts(users []model.UserEntry, keys []litellm.Key, gwUsers []litellm.User, bindings map[string]model.Binding) []accountRow
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/adminweb/accounts_test.go`:

```go
package adminweb

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func f64(v float64) *float64 { return &v }

func flagLabels(r accountRow) []string {
	var out []string
	for _, f := range r.Flags {
		out = append(out, f.Label)
	}
	return out
}

func TestReconcileAccountsJoinsAllThreeSources(t *testing.T) {
	users := []model.UserEntry{{WindowsUser: "alice", Name: "Alice", Department: "研发", Enabled: true}}
	keys := []litellm.Key{{KeyAlias: "emp-alice", UserID: "emp-alice", Models: []string{"glm-5.2"}}}
	gw := []litellm.User{{UserID: "emp-alice", Spend: 4.5, MaxBudget: f64(20), BudgetResetAt: "2026-10-01T00:00:00Z", Models: []string{"glm-5.2"}}}
	bindings := map[string]model.Binding{"PC-1": {User: "Alice"}}

	rows := reconcileAccounts(users, keys, gw, bindings)
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	r := rows[0]
	if !r.HasUser || !r.HasToken || r.Spend != 4.5 || r.Budget != 20 || r.BudgetResetAt == "" {
		t.Errorf("gateway state not joined: %+v", r)
	}
	if len(r.Machines) != 1 || r.Machines[0] != "PC-1" {
		t.Errorf("bindings not joined (case-insensitively): %v", r.Machines)
	}
	if len(r.Flags) != 0 {
		t.Errorf("a healthy account has no flags: %v", flagLabels(r))
	}
}

func TestReconcileAccountsFlagsEmployedWithoutToken(t *testing.T) {
	rows := reconcileAccounts([]model.UserEntry{{WindowsUser: "bob", Enabled: true}}, nil, nil, nil)
	if got := flagLabels(rows[0]); len(got) != 1 || got[0] != "在职无令牌" || rows[0].Flags[0].Fix != "onboard" {
		t.Errorf("flags: %+v", rows[0].Flags)
	}
}

func TestReconcileAccountsFlagsDepartedWithTokenOrMachines(t *testing.T) {
	users := []model.UserEntry{{WindowsUser: "carol", Enabled: false}}
	keys := []litellm.Key{{KeyAlias: "emp-carol", UserID: "emp-carol"}}
	bindings := map[string]model.Binding{"PC-9": {User: "carol"}}
	rows := reconcileAccounts(users, keys, nil, bindings)
	got := flagLabels(rows[0])
	if len(got) != 2 || got[0] != "已离职仍有令牌" || got[1] != "已离职仍有机器绑定" {
		t.Errorf("flags: %v", got)
	}
	for _, f := range rows[0].Flags {
		if f.Fix != "offboard" || f.Severity != "bad" {
			t.Errorf("both are fixed by offboarding and are severe: %+v", f)
		}
	}
}

func TestReconcileAccountsFlagsTokenWithoutUser(t *testing.T) {
	// The pre-feature state: a token minted before users existed.
	users := []model.UserEntry{{WindowsUser: "weipeng", Enabled: true}}
	keys := []litellm.Key{{KeyAlias: "emp-weipeng", Models: []string{"glm-5.2"}}}
	rows := reconcileAccounts(users, keys, nil, nil)
	got := flagLabels(rows[0])
	if len(got) != 1 || got[0] != "有令牌无网关用户" || rows[0].Flags[0].Fix != "onboard" {
		t.Errorf("flags: %+v", rows[0].Flags)
	}
	if len(rows[0].Models) != 1 {
		t.Errorf("without a user the token's allowlist is the best we have: %v", rows[0].Models)
	}
}

func TestReconcileAccountsSurfacesTokensWithNoRosterEntry(t *testing.T) {
	rows := reconcileAccounts(nil, []litellm.Key{{KeyAlias: "emp-ghost"}, {KeyAlias: "windows-control"}}, nil, nil)
	if len(rows) != 1 || rows[0].WindowsUser != "ghost" || rows[0].Enabled {
		t.Fatalf("stray employee token must appear as a departed row; admin keys must not: %+v", rows)
	}
	if got := flagLabels(rows[0]); len(got) != 1 || got[0] != "已离职仍有令牌" {
		t.Errorf("flags: %v", got)
	}
}

func TestReconcileAccountsSortsByUser(t *testing.T) {
	rows := reconcileAccounts([]model.UserEntry{{WindowsUser: "zed"}, {WindowsUser: "amy"}}, nil, nil, nil)
	if rows[0].WindowsUser != "amy" || rows[1].WindowsUser != "zed" {
		t.Errorf("order: %v %v", rows[0].WindowsUser, rows[1].WindowsUser)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/adminweb/ -run ReconcileAccounts -v`
Expected: undefined `reconcileAccounts`.

- [ ] **Step 3: Implement**

Create `internal/adminweb/accounts.go`:

```go
package adminweb

import (
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// accountRow is one employee as the account page shows them: the roster's
// view joined with the gateway's and the machine bindings'.
//
// Three sources, one row, so that a half-finished onboarding or offboarding
// is visible as a row with a flag rather than as three pages that each
// look fine on their own.
type accountRow struct {
	WindowsUser string
	Name        string
	Department  string
	Enabled     bool // still employed, per the roster

	HasUser  bool // the gateway has an internal user for this person
	HasToken bool // the gateway holds a token under this person's alias
	Models   []string

	Spend         float64
	Budget        float64
	BudgetResetAt string
	Quota         litellm.Quota

	Machines []string // bound machines, sorted
	Flags    []accountFlag
}

// accountFlag names one inconsistency and the action that clears it. The
// list page renders Fix as the button next to the flag.
type accountFlag struct {
	Label    string
	Severity string // "warn" or "bad"
	Fix      string // "onboard" or "offboard"
}

var (
	flagNoToken       = accountFlag{Label: "在职无令牌", Severity: "warn", Fix: "onboard"}
	flagNoUser        = accountFlag{Label: "有令牌无网关用户", Severity: "warn", Fix: "onboard"}
	flagDepartedToken = accountFlag{Label: "已离职仍有令牌", Severity: "bad", Fix: "offboard"}
	flagDepartedBound = accountFlag{Label: "已离职仍有机器绑定", Severity: "bad", Fix: "offboard"}
)

// reconcileAccounts pairs the roster against gateway users, gateway tokens
// and machine bindings. gwUsers or keys may be nil when the gateway was
// unreachable; the rows then carry roster and binding state only.
func reconcileAccounts(users []model.UserEntry, keys []litellm.Key, gwUsers []litellm.User, bindings map[string]model.Binding) []accountRow {
	keyByAlias := make(map[string]litellm.Key, len(keys))
	for _, k := range keys {
		if k.KeyAlias != "" {
			keyByAlias[k.KeyAlias] = k
		}
	}
	userByID := make(map[string]litellm.User, len(gwUsers))
	for _, u := range gwUsers {
		userByID[u.UserID] = u
	}
	machinesOf := map[string][]string{}
	for machine, b := range bindings {
		id := admincore.KeyAlias(b.User)
		machinesOf[id] = append(machinesOf[id], machine)
	}

	build := func(e model.UserEntry) accountRow {
		id := admincore.KeyAlias(e.WindowsUser)
		r := accountRow{WindowsUser: e.WindowsUser, Name: e.Name, Department: e.Department, Enabled: e.Enabled}
		if u, ok := userByID[id]; ok {
			r.HasUser = true
			r.Models = u.Models
			r.Spend = u.Spend
			r.Quota = u.Quota()
			r.Budget = r.Quota.MonthlyBudgetUSD
			r.BudgetResetAt = u.BudgetResetAt
		}
		if k, ok := keyByAlias[id]; ok {
			r.HasToken = true
			if r.Models == nil {
				r.Models = k.Models
			}
		}
		r.Machines = machinesOf[id]
		sort.Strings(r.Machines)

		switch {
		case e.Enabled && !r.HasToken:
			r.Flags = append(r.Flags, flagNoToken)
		case e.Enabled && r.HasToken && !r.HasUser:
			r.Flags = append(r.Flags, flagNoUser)
		}
		if !e.Enabled && r.HasToken {
			r.Flags = append(r.Flags, flagDepartedToken)
		}
		if !e.Enabled && len(r.Machines) > 0 {
			r.Flags = append(r.Flags, flagDepartedBound)
		}
		return r
	}

	out := make([]accountRow, 0, len(users))
	claimed := map[string]bool{}
	for _, e := range users {
		claimed[admincore.KeyAlias(e.WindowsUser)] = true
		out = append(out, build(e))
	}
	// Employee tokens whose alias matches nobody on the roster. No roster
	// row would ever show them, and they keep working until revoked.
	for alias := range keyByAlias {
		if claimed[alias] || !strings.HasPrefix(alias, "emp-") {
			continue
		}
		out = append(out, build(model.UserEntry{WindowsUser: strings.TrimPrefix(alias, "emp-"), Enabled: false}))
	}

	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].WindowsUser) < strings.ToLower(out[j].WindowsUser)
	})
	return out
}
```

Remove `gatewayHolder` and `reconcile` from `gateway.go` and the four `TestReconcile*` tests from `gateway_test.go` (keep `TestGatewayRefusesWithoutConfiguration` and `TestContextWindowLabel`). Remove `GatewayHolders` from `pageData` and the `reconcile(...)` call in `handleGateway` (leave the `ListKeys` call out entirely; the gateway page no longer needs keys).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/adminweb/ -run 'ReconcileAccounts|Gateway|ContextWindow' -v`
Expected: PASS (other adminweb tests may still fail to compile until Task 10; if `go vet` blocks, finish Task 10 before committing both together — otherwise commit now).

- [ ] **Step 5: Commit**

```bash
git add internal/adminweb/accounts.go internal/adminweb/accounts_test.go internal/adminweb/gateway.go internal/adminweb/gateway_test.go internal/adminweb/handlers.go
git commit -m "console: reconcile roster, gateway users, tokens and bindings into account rows"
```

---

### Task 10: Account pages, actions and routes

**Files:**
- Modify: `internal/adminweb/accounts.go` (handlers + actions)
- Modify: `internal/adminweb/handlers.go` (`pageData`, remove `handleUsers`)
- Modify: `internal/adminweb/actions.go` (remove `actionUserAdd`, `actionUserEnabled`)
- Modify: `internal/adminweb/gateway.go` (remove `actionGatewayProvision`, `actionGatewayRevoke`)
- Modify: `internal/adminweb/server.go` (routes, template funcs)
- Rewrite: `internal/adminweb/assets/users.html`; Create: `internal/adminweb/assets/user.html`
- Modify: `internal/adminweb/assets/_layout.html:23` (label), `internal/adminweb/assets/static/app.css`
- Modify: `internal/adminweb/actions_test.go` (CSRF list), `internal/adminweb/server_test.go:421` (users.html render test)

**Interfaces:**
- Consumes: `admincore.Onboard/Offboard/SetQuota/SetModels/Reissue`, `reconcileAccounts`, `litellm.Quota`.
- Produces routes: `GET /users`, `GET /users/detail?user=`, `POST /users/onboard`, `POST /users/offboard`, `POST /users/quota`, `POST /users/models`, `POST /users/reissue`.
- Produces template funcs: `usagepct(spend, budget float64) int` (0–100, clamped), `usagesev(spend, budget float64) string` ("s-ok" <80%, "s-warn" 80–99%, "s-bad" ≥100%), `money(v float64) string` (`%.2f`).

- [ ] **Step 1: Write the failing tests**

In `actions_test.go` `TestWritesRequireCSRFToken`, replace the two `/users/*` entries with:

```go
		{"/users/onboard", url.Values{"windowsUser": {"mallory"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"200000"}, "parallel": {"4"}}},
		{"/users/offboard", url.Values{"windowsUser": {"work1"}}},
		{"/users/quota", url.Values{"windowsUser": {"work1"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"200000"}, "parallel": {"4"}}},
		{"/users/models", url.Values{"windowsUser": {"work1"}}},
		{"/users/reissue", url.Values{"windowsUser": {"work1"}}},
		{"/settings/quota-defaults", url.Values{"budget": {"20"}, "rpm": {"60"}, "tpm": {"200000"}, "parallel": {"4"}}},
```

Append to `accounts_test.go`:

```go
func TestParseQuotaForm(t *testing.T) {
	good := url.Values{"budget": {"20.5"}, "rpm": {"60"}, "tpm": {"200000"}, "parallel": {"4"}}
	q, err := parseQuotaForm(good)
	if err != nil || q != (litellm.Quota{MonthlyBudgetUSD: 20.5, RPM: 60, TPM: 200000, Parallel: 4}) {
		t.Fatalf("good form: %+v %v", q, err)
	}
	for name, bad := range map[string]url.Values{
		"missing":  {"budget": {"20"}},
		"zero":     {"budget": {"0"}, "rpm": {"60"}, "tpm": {"1"}, "parallel": {"1"}},
		"negative": {"budget": {"20"}, "rpm": {"-1"}, "tpm": {"1"}, "parallel": {"1"}},
		"text":     {"budget": {"abc"}, "rpm": {"1"}, "tpm": {"1"}, "parallel": {"1"}},
	} {
		if _, err := parseQuotaForm(bad); err == nil {
			t.Errorf("%s form accepted", name)
		}
	}
}

func TestUsageHelpers(t *testing.T) {
	for _, tc := range []struct {
		spend, budget float64
		pct           int
		sev           string
	}{
		{0, 20, 0, "s-ok"},
		{10, 20, 50, "s-ok"},
		{16, 20, 80, "s-warn"},
		{20, 20, 100, "s-bad"},
		{35, 20, 100, "s-bad"},
		{5, 0, 0, "s-ok"},
	} {
		if got := usagePercent(tc.spend, tc.budget); got != tc.pct {
			t.Errorf("usagePercent(%v,%v) = %d, want %d", tc.spend, tc.budget, got, tc.pct)
		}
		if got := usageSeverity(tc.spend, tc.budget); got != tc.sev {
			t.Errorf("usageSeverity(%v,%v) = %s, want %s", tc.spend, tc.budget, got, tc.sev)
		}
	}
}

func TestAccountPagesRender(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	row := accountRow{WindowsUser: "alice", Name: "Alice", Department: "研发", Enabled: true,
		HasUser: true, HasToken: true, Models: []string{"glm-5.2"}, Spend: 17, Budget: 20,
		BudgetResetAt: "2026-10-01T00:00:00Z", Quota: litellm.Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4},
		Machines: []string{"PC-1"}}
	flagged := accountRow{WindowsUser: "carol", Enabled: false, HasToken: true,
		Flags: []accountFlag{flagDepartedToken}}
	data := pageData{CSRF: "x", GatewayEnabled: true, GatewayURL: "https://gw.example",
		GatewayModels: []litellm.Model{{Name: "glm-5.2", Info: litellm.ModelInfo{DisplayName: "GLM"}}},
		Accounts:      []accountRow{row, flagged}, QuotaDefaults: admincore.DefaultQuota}

	var list strings.Builder
	if err := s.tpl.ExecuteTemplate(&list, "users.html", data); err != nil {
		t.Fatalf("users.html: %v", err)
	}
	for _, want := range []string{"Alice", "研发", "17.00", "20.00", "s-warn w85", "已离职仍有令牌", "/users/offboard", "/users/onboard", `href="/users/detail?user=alice"`} {
		if !strings.Contains(list.String(), want) {
			t.Errorf("users.html missing %q", want)
		}
	}

	data.Account = &row
	data.Audit = []admincore.AuditEntry{{At: "2026-09-07T01:02:03Z", Action: admincore.AuditOnboard, User: "alice"}}
	var detail strings.Builder
	if err := s.tpl.ExecuteTemplate(&detail, "user.html", data); err != nil {
		t.Fatalf("user.html: %v", err)
	}
	for _, want := range []string{"/users/quota", "/users/models", "/users/reissue", "/users/offboard", "onboard", "PC-1", `value="60"`} {
		if !strings.Contains(detail.String(), want) {
			t.Errorf("user.html missing %q", want)
		}
	}
}
```

(Add `"net/url"`, `"strings"`, and the `admincore` import to `accounts_test.go`.)

Update the render assertion at `server_test.go:421` — it executes `users.html` with `pageData{Users: ...}`; change that fixture to `pageData{Accounts: []accountRow{{WindowsUser: "work1", Enabled: true}}}` and keep whatever string it asserts (`work1`).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/adminweb/ 2>&1 | head -20`
Expected: undefined `parseQuotaForm`, `usagePercent`, `Accounts`, template `user.html` missing.

- [ ] **Step 3: pageData and template funcs**

In `handlers.go` `pageData`, remove `Users []model.UserEntry` and `GatewayHolders`, add:

```go
	// Account pages.
	Accounts      []accountRow
	Account       *accountRow            // the detail page's subject
	Audit         []admincore.AuditEntry // that account's history, newest first
	QuotaDefaults litellm.Quota          // pre-fills the onboarding form
```

Delete `handleUsers` from `handlers.go`.

In `server.go` add to the `FuncMap`:

```go
		"usagepct": usagePercent,
		"usagesev": usageSeverity,
		"money":    func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
```

(import `strconv`). Add to `accounts.go`:

```go
// usagePercent is spend as a share of budget, clamped to 0..100 so it can
// select one of the .w0-.w100 width classes (inline styles are not allowed
// by the console's CSP).
func usagePercent(spend, budget float64) int {
	if budget <= 0 || spend <= 0 {
		return 0
	}
	pct := int(spend / budget * 100)
	if pct > 100 {
		return 100
	}
	return pct
}

// usageSeverity colours the bar: green under 80%, amber to 99%, red at or
// over budget -- the gateway is refusing requests at that point.
func usageSeverity(spend, budget float64) string {
	switch pct := usagePercent(spend, budget); {
	case pct >= 100:
		return "s-bad"
	case pct >= 80:
		return "s-warn"
	default:
		return "s-ok"
	}
}
```

- [ ] **Step 4: Handlers and actions**

Append to `accounts.go` (imports: `context`, `fmt`, `log`, `net/http`, `net/url`, `strconv`, `strings`, `time`):

```go
// handleUsers is the account list: the roster joined with the gateway.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "users")
	data.GatewayURL = s.opts.GatewayURL

	us, err := sess.mgr.LoadUsers()
	if err != nil {
		data.Error = "could not read the roster"
		log.Printf("adminweb: LoadUsers: %v", err)
		s.render(w, "users.html", http.StatusOK, data)
		return
	}
	bindings, err := sess.mgr.ListBindings()
	if err != nil {
		log.Printf("adminweb: ListBindings: %v", err)
	}
	if q, err := sess.mgr.LoadQuotaDefaults(); err == nil {
		data.QuotaDefaults = q
	}

	var keys []litellm.Key
	var gwUsers []litellm.User
	gw, err := s.gateway()
	if err != nil {
		data.GatewayUnusable = err.Error()
	} else {
		data.GatewayEnabled = true
		ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
		defer cancel()
		// Each failure is reported once and the page still renders the
		// roster: knowing the gateway is down is the point, not a blocker.
		if data.GatewayModels, err = gw.Models(ctx); err != nil {
			data.GatewayUnusable = "无法读取网关模型清单：" + err.Error()
		}
		if keys, err = gw.ListKeys(ctx); err != nil && data.GatewayUnusable == "" {
			data.GatewayUnusable = "无法读取网关令牌清单：" + err.Error()
		}
		if gwUsers, err = gw.ListUsers(ctx); err != nil && data.GatewayUnusable == "" {
			data.GatewayUnusable = "无法读取网关用户清单：" + err.Error()
		}
		if data.GatewayUnusable != "" {
			log.Printf("adminweb: accounts: %s", data.GatewayUnusable)
			data.GatewayEnabled = false
		}
	}
	data.Accounts = reconcileAccounts(us.Users, keys, gwUsers, bindings)
	s.render(w, "users.html", http.StatusOK, data)
}

// handleUserDetail is one account: editing forms and history.
func (s *Server) handleUserDetail(w http.ResponseWriter, r *http.Request, sess *session) {
	user := strings.TrimSpace(r.URL.Query().Get("user"))
	if user == "" {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	data := newPage(sess, r, "users")
	data.GatewayURL = s.opts.GatewayURL

	us, err := sess.mgr.LoadUsers()
	if err != nil {
		data.Error = "could not read the roster"
		s.render(w, "user.html", http.StatusOK, data)
		return
	}
	e := us.Find(user)
	if e == nil {
		http.NotFound(w, r)
		return
	}
	bindings, _ := sess.mgr.ListBindings()
	data.Audit, _ = sess.mgr.ReadAudit(e.WindowsUser)

	var keys []litellm.Key
	var gwUsers []litellm.User
	if gw, err := s.gateway(); err != nil {
		data.GatewayUnusable = err.Error()
	} else {
		data.GatewayEnabled = true
		ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
		defer cancel()
		if data.GatewayModels, err = gw.Models(ctx); err != nil {
			data.GatewayUnusable = "无法读取网关模型清单：" + err.Error()
		}
		if k, found, err := gw.FindKeyByAlias(ctx, admincore.KeyAlias(e.WindowsUser)); err != nil {
			data.GatewayUnusable = "无法读取网关令牌：" + err.Error()
		} else if found {
			keys = []litellm.Key{k}
		}
		if u, found, err := gw.UserInfo(ctx, admincore.KeyAlias(e.WindowsUser)); err != nil {
			data.GatewayUnusable = "无法读取网关用户：" + err.Error()
		} else if found {
			gwUsers = []litellm.User{u}
		}
		if data.GatewayUnusable != "" {
			data.GatewayEnabled = false
		}
	}
	rows := reconcileAccounts([]model.UserEntry{*e}, keys, gwUsers, bindings)
	data.Account = &rows[0]
	s.render(w, "user.html", http.StatusOK, data)
}

// parseQuotaForm reads the four quota fields. All are required: see
// litellm.Quota for why zero cannot mean unlimited.
func parseQuotaForm(form url.Values) (litellm.Quota, error) {
	var q litellm.Quota
	var err error
	if q.MonthlyBudgetUSD, err = strconv.ParseFloat(strings.TrimSpace(form.Get("budget")), 64); err != nil {
		return q, fmt.Errorf("月预算需要是一个数字")
	}
	ints := map[string]*int{"rpm": &q.RPM, "tpm": &q.TPM, "parallel": &q.Parallel}
	labels := map[string]string{"rpm": "每分钟请求数", "tpm": "每分钟 token 数", "parallel": "并发数"}
	for name, dst := range ints {
		if *dst, err = strconv.Atoi(strings.TrimSpace(form.Get(name))); err != nil {
			return q, fmt.Errorf("%s需要是一个整数", labels[name])
		}
	}
	if err := q.Validate(); err != nil {
		return q, fmt.Errorf("额度必须都大于 0")
	}
	return q, nil
}

func (s *Server) accountContext(r *http.Request) (*litellm.Client, admincore.GatewayConfig, context.Context, context.CancelFunc, error) {
	gw, err := s.gateway()
	if err != nil {
		return nil, admincore.GatewayConfig{}, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
	return gw, admincore.GatewayConfig{BaseURL: s.opts.GatewayURL}, ctx, cancel, nil
}

func (s *Server) actionAccountOnboard(sess *session, r *http.Request) error {
	gw, cfg, ctx, cancel, err := s.accountContext(r)
	if err != nil {
		return err
	}
	defer cancel()
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}
	quota, err := parseQuotaForm(r.PostForm)
	if err != nil {
		return err
	}
	return sess.mgr.Onboard(ctx, gw, cfg, admincore.AccountSpec{
		WindowsUser: user,
		Name:        formValue(r, "name"),
		Department:  formValue(r, "department"),
		Quota:       quota,
		Models:      r.PostForm["models"], // none selected = everything the gateway offers
	})
}

// actionAccountReopen is onboarding from a row button: no form fields
// beyond the user, so labels and quota come from the roster and the
// existing gateway user (or the defaults when there is none).
func (s *Server) actionAccountReopen(sess *session, r *http.Request) error {
	gw, cfg, ctx, cancel, err := s.accountContext(r)
	if err != nil {
		return err
	}
	defer cancel()
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}
	us, err := sess.mgr.LoadUsers()
	if err != nil {
		return err
	}
	e := us.Find(user)
	if e == nil {
		return fmt.Errorf("user %q not found in roster", user)
	}
	quota, err := sess.mgr.LoadQuotaDefaults()
	if err != nil {
		return err
	}
	var models []string
	if u, found, err := gw.UserInfo(ctx, admincore.KeyAlias(e.WindowsUser)); err != nil {
		return fmt.Errorf("look up gateway user: %w", err)
	} else if found {
		if q := u.Quota(); q.Validate() == nil {
			quota = q
		}
		models = u.Models
	}
	return sess.mgr.Onboard(ctx, gw, cfg, admincore.AccountSpec{
		WindowsUser: e.WindowsUser, Name: e.Name, Department: e.Department, Quota: quota, Models: models,
	})
}

func (s *Server) actionAccountOffboard(sess *session, r *http.Request) error {
	gw, _, ctx, cancel, err := s.accountContext(r)
	if err != nil {
		return err
	}
	defer cancel()
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}
	return sess.mgr.Offboard(ctx, gw, user)
}

func (s *Server) actionAccountQuota(sess *session, r *http.Request) error {
	gw, _, ctx, cancel, err := s.accountContext(r)
	if err != nil {
		return err
	}
	defer cancel()
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}
	quota, err := parseQuotaForm(r.PostForm)
	if err != nil {
		return err
	}
	return sess.mgr.SetQuota(ctx, gw, user, quota)
}

func (s *Server) actionAccountModels(sess *session, r *http.Request) error {
	gw, cfg, ctx, cancel, err := s.accountContext(r)
	if err != nil {
		return err
	}
	defer cancel()
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}
	return sess.mgr.SetModels(ctx, gw, cfg, user, r.PostForm["models"])
}

func (s *Server) actionAccountReissue(sess *session, r *http.Request) error {
	gw, cfg, ctx, cancel, err := s.accountContext(r)
	if err != nil {
		return err
	}
	defer cancel()
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}
	return sess.mgr.Reissue(ctx, gw, cfg, user)
}

// backToAccount sends a detail-page form back to the detail page, and a
// list-page form back to the list. The form says which with a hidden field.
func backToAccount(r *http.Request) string {
	if r.PostFormValue("back") == "detail" {
		return "/users/detail?user=" + url.QueryEscape(r.PostFormValue("windowsUser"))
	}
	return "/users"
}
```

`requirePost` takes a fixed `back` path. Add a sibling in `actions.go` that computes it per request:

```go
// requirePostBack is requirePost with the return page chosen by the form
// (see backToAccount): the same action is posted from the list and from a
// detail page, and each should land back where it started.
func (s *Server) requirePostBack(back func(*http.Request) string, next func(*session, *http.Request) error) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request, sess *session) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/users", http.StatusSeeOther)
			return
		}
		if err := parseUpload(r); err != nil {
			s.redirectWithError(w, r, "/users", "could not read the form")
			return
		}
		dest := back(r)
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
			log.Printf("adminweb: rejected a POST to %s with a bad CSRF token from %s", r.URL.Path, s.clientKey(r))
			s.redirectWithError(w, r, dest, "the form expired; reload the page and try again")
			return
		}
		if err := next(sess, r); err != nil {
			log.Printf("adminweb: %s: %v", r.URL.Path, err)
			s.redirectWithError(w, r, dest, err.Error())
			return
		}
		sep := "?"
		if strings.Contains(dest, "?") {
			sep = "&"
		}
		http.Redirect(w, r, dest+sep+"ok=1", http.StatusSeeOther)
	})
}
```

Delete `actionUserAdd` and `actionUserEnabled` from `actions.go`, and `actionGatewayProvision`/`actionGatewayRevoke` from `gateway.go`.

In `server.go` replace the `/users/*` and `/gateway/*` POST routes with:

```go
	mux.HandleFunc("/users/detail", s.requireSession(s.handleUserDetail))
	mux.HandleFunc("/users/onboard", s.requirePostBack(backToAccount, s.actionAccountOnboard))
	mux.HandleFunc("/users/reopen", s.requirePostBack(backToAccount, s.actionAccountReopen))
	mux.HandleFunc("/users/offboard", s.requirePostBack(backToAccount, s.actionAccountOffboard))
	mux.HandleFunc("/users/quota", s.requirePostBack(backToAccount, s.actionAccountQuota))
	mux.HandleFunc("/users/models", s.requirePostBack(backToAccount, s.actionAccountModels))
	mux.HandleFunc("/users/reissue", s.requirePostBack(backToAccount, s.actionAccountReissue))
```

Add `{"/users/reopen", url.Values{"windowsUser": {"work1"}}}` to the CSRF test list too.

- [ ] **Step 5: Templates**

Rewrite `internal/adminweb/assets/users.html`:

```html
{{define "users.html"}}{{template "head" "员工账号"}}
<body>
<div class="app">
{{template "nav" .}}
<main class="sheet">
  <header class="pagehead"><h1>员工账号</h1></header>
  {{template "notices" .}}
  {{if .GatewayUnusable}}<p class="note note-err">{{.GatewayUnusable}}<br><span class="dim">额度与令牌列暂不可用；关户仍可执行。</span></p>{{end}}

  <div class="panel scroll">
  <table>
    <thead><tr><th>WINDOWS 用户</th><th>姓名</th><th>部门</th><th>状态</th><th>令牌</th><th>可用模型</th><th>本月花费 / 预算</th><th>速率</th><th></th></tr></thead>
    <tbody>
    {{range .Accounts}}
      <tr>
        <td class="id"><a href="/users/detail?user={{.WindowsUser}}">{{.WindowsUser}}</a></td>
        <td>{{if .Name}}{{.Name}}{{else}}<span class="dim">—</span>{{end}}</td>
        <td class="dim">{{if .Department}}{{.Department}}{{else}}—{{end}}</td>
        <td>
          {{if .Enabled}}<span class="tag tag-ok">在职</span>{{else}}<span class="tag tag-bad">已离职</span>{{end}}
          {{range .Flags}}<span class="tag tag-{{.Severity}}">{{.Label}}</span>{{end}}
        </td>
        <td>{{if .HasToken}}<span class="tag tag-ok">已发放</span>{{else}}<span class="dim">—</span>{{end}}</td>
        <td class="dim">{{if .Models}}{{range $i, $m := .Models}}{{if $i}}, {{end}}{{$m}}{{end}}{{else}}—{{end}}</td>
        <td>
          {{if .HasUser}}
          <div class="usage"><div class="bar"><span class="{{usagesev .Spend .Budget}} w{{usagepct .Spend .Budget}}"></span></div>
            <span class="dim">${{money .Spend}} / ${{money .Budget}}{{if .BudgetResetAt}} · {{slice .BudgetResetAt 0 10}} 重置{{end}}</span></div>
          {{else}}<span class="dim">—</span>{{end}}
        </td>
        <td class="dim">{{if .HasUser}}{{.Quota.RPM}} rpm · {{.Quota.TPM}} tpm · {{.Quota.Parallel}} 并发{{else}}—{{end}}</td>
        <td>
          {{if .Enabled}}
          <form method="post" action="/users/offboard" class="inline">
            <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{.WindowsUser}}">
            <button class="link danger">关户</button>
          </form>
          {{else}}
          <form method="post" action="/users/reopen" class="inline">
            <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{.WindowsUser}}">
            <button class="link"{{if not $.GatewayEnabled}} disabled{{end}}>重新开户</button>
          </form>
          {{end}}
          {{range .Flags}}{{if and (eq .Fix "onboard") $.GatewayEnabled}}
          <form method="post" action="/users/reopen" class="inline">
            <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{$.WindowsUser}}">
            <button class="link">修复</button>
          </form>
          {{end}}{{end}}
        </td>
      </tr>
    {{else}}
      <tr><td colspan="9" class="empty">还没有员工。</td></tr>
    {{end}}
    </tbody>
  </table>
  </div>

  <h2>开户</h2>
  <form method="post" action="/users/onboard" class="form">
    <input type="hidden" name="csrf" value="{{.CSRF}}">
    <label>Windows 用户<input name="windowsUser" required placeholder="work1"></label>
    <label>姓名<input name="name" placeholder="张三"></label>
    <label>部门<input name="department" placeholder="研发"></label>
    <label>月预算（美元）<input name="budget" type="number" step="0.01" min="0.01" value="{{.QuotaDefaults.MonthlyBudgetUSD}}" required></label>
    <label>每分钟请求数<input name="rpm" type="number" min="1" value="{{.QuotaDefaults.RPM}}" required></label>
    <label>每分钟 token 数<input name="tpm" type="number" min="1" value="{{.QuotaDefaults.TPM}}" required></label>
    <label>并发数<input name="parallel" type="number" min="1" value="{{.QuotaDefaults.Parallel}}" required></label>
    <fieldset class="checks">
      <legend>可用模型（全不选 = 网关上的全部）</legend>
      {{range .GatewayModels}}
        <label class="check"><input type="checkbox" name="models" value="{{.Name}}"> {{.Info.DisplayName}} <span class="dim">{{.Name}}</span></label>
      {{end}}
    </fieldset>
    <button type="submit"{{if not .GatewayEnabled}} disabled{{end}}>开户并下发配置</button>
  </form>
  <p class="hint">
    开户一次完成：入名册、在网关建用户与令牌、生成并下发 Codex 配置。机器绑定在<a href="/machines">机器</a>页。
    <strong>员工需要完全退出所有 Codex 进程再重新打开</strong>——模型目录只在启动时加载一次。<br>
    预算按自然月重置；超额后网关直接拒绝请求。关户会即时吊销令牌、撤回配置、解绑机器，名册与用量历史保留。
  </p>
</main>
</div>
</body>
</html>{{end}}
```

The `{{range .Flags}}` inner form references `$.WindowsUser` which does not exist on `pageData`; use a `with`-scoped variable instead. Rewrite that cell's loop as:

```html
          {{$u := .WindowsUser}}{{range .Flags}}{{if and (eq .Fix "onboard") $.GatewayEnabled}}
          <form method="post" action="/users/reopen" class="inline">
            <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{$u}}">
            <button class="link">修复</button>
          </form>
          {{end}}{{end}}
```

Create `internal/adminweb/assets/user.html`:

```html
{{define "user.html"}}{{template "head" "员工账号"}}
<body>
<div class="app">
{{template "nav" .}}
<main class="sheet">
  <header class="pagehead"><h1><a href="/users" class="dim">员工账号</a> / {{with .Account}}{{.WindowsUser}}{{end}}</h1></header>
  {{template "notices" .}}
  {{if .GatewayUnusable}}<p class="note note-err">{{.GatewayUnusable}}</p>{{end}}

  {{with .Account}}
  <p>
    {{if .Enabled}}<span class="tag tag-ok">在职</span>{{else}}<span class="tag tag-bad">已离职</span>{{end}}
    {{range .Flags}}<span class="tag tag-{{.Severity}}">{{.Label}}</span>{{end}}
    {{if .Name}}{{.Name}}{{end}}{{if .Department}} · {{.Department}}{{end}}
    {{if .Machines}} · 机器：{{range $i, $m := .Machines}}{{if $i}}, {{end}}<code>{{$m}}</code>{{end}}{{end}}
  </p>

  {{if .HasUser}}
  <h2>本月用量</h2>
  <div class="usage wide"><div class="bar"><span class="{{usagesev .Spend .Budget}} w{{usagepct .Spend .Budget}}"></span></div>
    <span>${{money .Spend}} / ${{money .Budget}}{{if .BudgetResetAt}} · {{slice .BudgetResetAt 0 10}} 重置{{end}}</span></div>
  {{end}}

  <h2>额度</h2>
  <form method="post" action="/users/quota" class="form">
    <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{.WindowsUser}}"><input type="hidden" name="back" value="detail">
    <label>月预算（美元）<input name="budget" type="number" step="0.01" min="0.01" value="{{.Quota.MonthlyBudgetUSD}}" required></label>
    <label>每分钟请求数<input name="rpm" type="number" min="1" value="{{.Quota.RPM}}" required></label>
    <label>每分钟 token 数<input name="tpm" type="number" min="1" value="{{.Quota.TPM}}" required></label>
    <label>并发数<input name="parallel" type="number" min="1" value="{{.Quota.Parallel}}" required></label>
    <button type="submit"{{if or (not $.GatewayEnabled) (not .HasUser)}} disabled{{end}}>保存额度</button>
  </form>
  {{if not .HasUser}}<p class="hint">该员工在网关上还没有用户记录，先点「重新开户」建立。</p>{{end}}

  <h2>可用模型</h2>
  <form method="post" action="/users/models" class="form">
    <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{.WindowsUser}}"><input type="hidden" name="back" value="detail">
    <fieldset class="checks">
      <legend>全不选 = 网关上的全部</legend>
      {{$row := .}}{{range $.GatewayModels}}
        <label class="check"><input type="checkbox" name="models" value="{{.Name}}"{{range $row.Models}}{{if eq . $.Name}} checked{{end}}{{end}}> {{.Info.DisplayName}} <span class="dim">{{.Name}}</span></label>
      {{end}}
    </fieldset>
    <button type="submit"{{if or (not $.GatewayEnabled) (not .HasToken)}} disabled{{end}}>保存模型并重发目录</button>
  </form>

  <h2>令牌</h2>
  <div class="row">
    <form method="post" action="/users/reissue" class="inline">
      <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{.WindowsUser}}"><input type="hidden" name="back" value="detail">
      <button{{if or (not $.GatewayEnabled) (not .Enabled)}} disabled{{end}}>重发令牌并下发配置</button>
    </form>
    {{if .Enabled}}
    <form method="post" action="/users/offboard" class="inline">
      <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{.WindowsUser}}"><input type="hidden" name="back" value="detail">
      <button class="danger">关户</button>
    </form>
    {{else}}
    <form method="post" action="/users/reopen" class="inline">
      <input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="windowsUser" value="{{.WindowsUser}}"><input type="hidden" name="back" value="detail">
      <button{{if not $.GatewayEnabled}} disabled{{end}}>重新开户</button>
    </form>
    {{end}}
  </div>
  {{end}}

  <h2>操作记录</h2>
  <div class="panel scroll">
  <table>
    <thead><tr><th>时间</th><th>动作</th><th>详情</th></tr></thead>
    <tbody>
    {{range .Audit}}
      <tr><td class="dim">{{.At}}</td><td class="id">{{.Action}}</td><td class="dim">{{range $k, $v := .Detail}}{{$k}}={{$v}} {{end}}</td></tr>
    {{else}}
      <tr><td colspan="3" class="empty">还没有记录。</td></tr>
    {{end}}
    </tbody>
  </table>
  </div>
</main>
</div>
</body>
</html>{{end}}
```

The checkbox `checked` expression above compares `$.Name` incorrectly (`$` is `pageData`). Replace that `<label>` line with a helper-driven form:

```html
        <label class="check"><input type="checkbox" name="models" value="{{.Name}}"{{if has $row.Models .Name}} checked{{end}}> {{.Info.DisplayName}} <span class="dim">{{.Name}}</span></label>
```

and add to the `FuncMap`:

```go
		"has": func(list []string, v string) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		},
```

Change `_layout.html:23` label from `员工` to `员工账号`.

Append to `app.css`:

```css
/* ---------- account usage bars: share .fleet's width classes ---------- */
.usage { display: flex; flex-direction: column; gap: 4px; min-width: 160px; }
.usage.wide { max-width: 480px; margin-bottom: 16px; }
.usage .bar {
  display: flex; height: 8px; border-radius: 4px; overflow: hidden;
  background: var(--panel-sunk); border: 1px solid var(--rule);
}
.usage .bar span { display: block; }
.usage .bar .s-ok { background: var(--ok); }
.usage .bar .s-warn { background: var(--warn); }
.usage .bar .s-bad { background: var(--bad); }
```

The `.w0`…`.w100` width rules are currently scoped as `.fleet .bar .wN`. Make them apply to both bars: run

```bash
sed -i 's/^\.fleet \.bar \.w\([0-9]\+\) {/.fleet .bar .w\1, .usage .bar .w\1 {/' internal/adminweb/assets/static/app.css
```

and check with `grep -c 'usage .bar .w' internal/adminweb/assets/static/app.css` → 101.

- [ ] **Step 6: Run everything**

Run: `go vet ./... && go test ./...`
Expected: PASS. If `TestRenderPreview` fixtures reference `Users` or `GatewayHolders`, update them to `Accounts` with one or two `accountRow` values.

- [ ] **Step 7: Commit**

```bash
git add internal/adminweb
git commit -m "console: employee account page with onboard, offboard, quota, models and reissue"
```

---

### Task 11: Slim gateway page, quota defaults form, nav

**Files:**
- Modify: `internal/adminweb/assets/gateway.html`, `internal/adminweb/assets/settings.html`
- Modify: `internal/adminweb/handlers.go` (`handleSettings` loads defaults), `internal/adminweb/actions.go` (`actionQuotaDefaults`), `internal/adminweb/server.go` (route)
- Modify: `internal/adminweb/actions_test.go` (already lists `/settings/quota-defaults`)

- [ ] **Step 1: Write the failing test**

Append to `accounts_test.go`:

```go
func TestQuotaDefaultsActionPersists(t *testing.T) {
	fs := newFakeStore()
	s := newTestServer(t, fs)
	cookie := signIn(t, s)
	rec := post(t, s, "/settings/quota-defaults", cookie, url.Values{
		"csrf": {csrfOf(t, s, cookie)}, "budget": {"33"}, "rpm": {"70"}, "tpm": {"250000"}, "parallel": {"5"},
	})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok=1") {
		t.Fatalf("expected redirect with ok, got %d %s", rec.Code, rec.Header().Get("Location"))
	}
	raw, ok := fs.objects[admincore.QuotaDefaultsKey()]
	if !ok || !strings.Contains(string(raw), `"monthlyBudgetUSD": 33`) {
		t.Errorf("defaults not saved: %s", raw)
	}
}
```

(import `net/http`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/adminweb/ -run QuotaDefaultsAction -v`
Expected: 404 or redirect without `ok=1`.

- [ ] **Step 3: Implement**

`actions.go`:

```go
func (s *Server) actionQuotaDefaults(sess *session, r *http.Request) error {
	q, err := parseQuotaForm(r.PostForm)
	if err != nil {
		return err
	}
	return sess.mgr.SaveQuotaDefaults(q)
}
```

`server.go` route: `mux.HandleFunc("/settings/quota-defaults", s.requirePost("/settings", s.actionQuotaDefaults))`.

`handleSettings`: after the stats block add

```go
	if q, err := sess.mgr.LoadQuotaDefaults(); err == nil {
		data.QuotaDefaults = q
	}
```

`settings.html`: before `{{if .Stats}}` insert

```html
  <h2>开户默认值</h2>
  <form method="post" action="/settings/quota-defaults" class="form">
    <input type="hidden" name="csrf" value="{{.CSRF}}">
    <label>月预算（美元）<input name="budget" type="number" step="0.01" min="0.01" value="{{.QuotaDefaults.MonthlyBudgetUSD}}" required></label>
    <label>每分钟请求数<input name="rpm" type="number" min="1" value="{{.QuotaDefaults.RPM}}" required></label>
    <label>每分钟 token 数<input name="tpm" type="number" min="1" value="{{.QuotaDefaults.TPM}}" required></label>
    <label>并发数<input name="parallel" type="number" min="1" value="{{.QuotaDefaults.Parallel}}" required></label>
    <button type="submit">保存</button>
  </form>
  <p class="hint">只影响之后新开的账号；已开账号在各自详情页调整。</p>
```

`gateway.html`: delete the whole `<h2>员工令牌</h2>` table and the `<h2>发放令牌</h2>` form and their hints; replace the trailing hint with:

```html
  <p class="hint">发放、吊销令牌与额度管理在<a href="/users">员工账号</a>页。</p>
```

- [ ] **Step 4: Run everything**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adminweb
git commit -m "console: quota defaults in settings; gateway page shows models only"
```

---

### Task 12: Gateway prices for glm-5.2 and deepseek-v4-flash

**Files:**
- Modify: `/root/pp_home/windows-pc/gateway-litellm/config.yaml` (local copy) and `~/gateway-litellm/config.yaml` on `ec2-user@175.41.186.104`
- Modify: `/root/pp_home/windows-pc/gateway-litellm/README.md` (decision log)

Prices derive from the relay's public table (`GET https://run.v3.cm/api/pricing`, new-api format: USD per 1M tokens = `model_ratio × 2 × group_ratio`, output = input × `completion_ratio`). Using group ratio 1 (the `default` group) as the conservative upper bound:

| model | model_ratio | completion_ratio | input $/token | output $/token |
|---|---|---|---|---|
| glm-5.2 | 4 | 3.5 | `0.000008` | `0.000028` |
| deepseek-v4-flash | 0.5 | 2 | `0.000001` | `0.000002` |

- [ ] **Step 1: Edit both config copies**

Under `glm-5.2` → `litellm_params`, add:

```yaml
      # 2026-09-07 按中转站定价表（/api/pricing, group_ratio 1）折算；没有单价花费恒为 0，预算形同虚设。
      input_cost_per_token: 0.000008
      output_cost_per_token: 0.000028
```

Under `deepseek-v4-flash` → `litellm_params`, add:

```yaml
      input_cost_per_token: 0.000001
      output_cost_per_token: 0.000002
```

Apply to the local copy with your editor, then copy to the host:

```bash
scp /root/pp_home/windows-pc/gateway-litellm/config.yaml ec2-user@175.41.186.104:~/gateway-litellm/config.yaml
ssh ec2-user@175.41.186.104 'cd ~/gateway-litellm && docker compose restart litellm && sleep 8 && K=$(grep LITELLM_MASTER_KEY .env | cut -d= -f2) && curl -s -H "Authorization: Bearer $K" http://127.0.0.1:4000/model/info | python3 -c "import sys,json; [print(m[\"model_name\"], m[\"model_info\"].get(\"input_cost_per_token\"), m[\"model_info\"].get(\"output_cost_per_token\")) for m in json.load(sys.stdin)[\"data\"]]"'
```

Expected output includes `glm-5.2 8e-06 2.8e-05` and `deepseek-v4-flash 1e-06 2e-06`.

- [ ] **Step 2: Verify spend is now non-zero for glm**

```bash
ssh ec2-user@175.41.186.104 'cd ~/gateway-litellm && K=$(grep LITELLM_MASTER_KEY .env | cut -d= -f2) && curl -s -X POST -H "Authorization: Bearer $K" -H "Content-Type: application/json" http://127.0.0.1:4000/v1/responses -d "{\"model\":\"glm-5.2\",\"input\":\"say hi\",\"max_output_tokens\":8}" >/dev/null; sleep 3; curl -s -H "Authorization: Bearer $K" "http://127.0.0.1:4000/spend/logs?limit=1" | python3 -c "import sys,json; l=json.load(sys.stdin)[0]; print(l[\"model\"], l[\"total_tokens\"], l[\"spend\"])"'
```

Expected: `openai/glm-5.2 <n> <positive number>`.

- [ ] **Step 3: Record in README**

Append to `/root/pp_home/windows-pc/gateway-litellm/README.md` under the decision log:

```markdown
### 2026-09-07 · 补 glm-5.2 / deepseek-v4-flash 单价
- 之前二者无 `input_cost_per_token`，花费恒 0，用户级预算对它们无效。
- 按中转站 `/api/pricing`（new-api：$/1M = model_ratio × 2 × group_ratio，output = × completion_ratio）以 group_ratio 1 折算：glm-5.2 8e-6 / 2.8e-5，deepseek-v4-flash 1e-6 / 2e-6。是上限近似，不是真实账单。
- 员工额度改挂 LiteLLM internal user（`emp-<user>`），令牌不再带 max_budget；见 `设计_员工账号管理与用量限制_2026-09.md`。
```

No git here (the folder is not a repository).

---

### Task 13: Build, deploy console web-31, acceptance

**Files:**
- Deploy target: `/opt/ai-env-mgr/web-31` on `47.236.115.50` (see how web-30 was deployed: `ls /opt/ai-env-mgr/` there and the systemd unit `ai-env-mgr-web.service` with drop-in `gateway.conf`).

- [ ] **Step 1: Build**

```bash
cd /root/pp_home/windows-pc/ai-env-mgr/go && go vet ./... && go test ./... && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../dist/admin-web-31 ./cmd/admin
```

- [ ] **Step 2: Deploy**

Copy the binary next to `web-30`, repoint the symlink/ExecStart the same way web-30 was installed, restart the unit, and confirm:

```bash
ssh root@47.236.115.50 'systemctl status ai-env-mgr-web --no-pager | head -5 && journalctl -u ai-env-mgr-web -n 5 --no-pager'
```

Expected: `active (running)`, no `adminweb:` errors in the last lines.

- [ ] **Step 3: Acceptance checklist (record results in the spec §8)**

1. Open `/users`. Row `weipeng` shows flag `有令牌无网关用户` with a `修复` button. Click it. Row now shows a usage bar `$0.00 / $20.00 · 2026-10-01 重置` and no flags.
2. Open `/users/detail?user=weipeng`. Set budget to `0.0001`, save. From a machine with the delivered token run two Codex turns: the second must fail with the gateway's `budget_exceeded` message. Restore the budget.
3. Set rpm to `1`, save; fire three requests within a minute with the employee token (curl `/v1/responses` with `max_output_tokens: 8`): the third must return 429. Restore rpm. **This closes the untested item in spec §1.**
4. Onboard a test account `work-test` with `glm-5.2` only. Bind a machine to it. Offboard it. Confirm: `/key/list` has no `emp-work-test`; `/users` row shows `已离职`, no flags; machine page shows the machine unbound; the gateway UI still lists user `emp-work-test` with its spend.
5. `/users/detail?user=work-test` shows `onboard` and `offboard` audit lines with timestamps.

- [ ] **Step 4: Tag**

```bash
cd /root/pp_home/windows-pc/ai-env-mgr/go && git tag -a web-31 -m "console: employee accounts with monthly budget and rate limits" && git push origin HEAD --tags
```

---

## Self-Review

**Spec coverage**
- §2 data model → Tasks 1, 2, 4, 5. ✔
- §3.1 Onboard → Task 6; §3.2 Offboard → Task 7; §3.3–3.5 → Task 8; §3.6 interfaces → Tasks 2, 3. ✔
- §4.1 list page, §4.2 detail → Task 10; §4.3 gateway slim, §4.4 settings → Task 11. ✔
- §5 audit → Task 5 (format, no operator, read-modify-write, failure only logged). ✔
- §6 flags and gateway-down behaviour → Tasks 9, 10 (offboard button stays enabled; onboard/quota/models/reissue disabled when `GatewayEnabled` is false). ✔
- §7 prices, weipeng migration via 修复/重新开户, no alerting → Tasks 12, 13. ✔
- §8 tests and acceptance → per task + Task 13. ✔
- §9 non-goals: no team, no token quota, no operator identity, no hard delete, no agent change. ✔

**Placeholder scan** — none; every step has code or an exact command.

**Type consistency** — `litellm.Quota{MonthlyBudgetUSD, RPM, TPM, Parallel}`, `UserSpec`, `User.Quota()`, `User.Department()`, `AccountSpec`, `accountRow`, `accountFlag{Label, Severity, Fix}`, `provisionLocked`, `setModelsLocked`, `ensureGatewayUser(ctx, gw, entry, *Quota, models)`, `DefaultQuota`, `QuotaDefaultsKey`, `AuditKey`, `auditReadLimit`, `parseQuotaForm`, `usagePercent`, `usageSeverity`, `requirePostBack`, `backToAccount` are used with the same names and signatures throughout.
