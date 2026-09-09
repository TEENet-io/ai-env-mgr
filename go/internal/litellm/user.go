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
	UserID     string // admincore.KeyAlias(windowsUser)
	Alias      string // display name, shown in the gateway UI
	Department string // stored as metadata.department
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
