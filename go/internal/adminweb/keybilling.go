package adminweb

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/worker"
)

// keyBillingPage is the read-only, per-token view of the usage snapshot.
// LocalCost is the selected month's immutable daily snapshot; GatewaySpend is
// the current aggregate LiteLLM reports for that key and may cover a different
// period. Keeping both visible prevents the two numbers being mistaken for
// interchangeable sources of truth.
type keyBillingPage struct {
	Month        string
	Months       []string
	Rows         []keyBillingRow
	Total        keyBillingTotal
	Query        string
	GatewayError string
}

type keyBillingTotal struct {
	Keys, Calls, Failures, Unpriced int
	Tokens                          int64
	LocalCost                       string
}

type keyBillingRow struct {
	Alias        string
	Employee     string
	Department   string
	Fingerprint  string
	LocalCost    string
	GatewaySpend string
	Calls        int
	Failures     int
	Tokens       int64
	Unpriced     int
	Models       string
	Status       string
	Live         bool
}

type keyBillingAggregate struct {
	Alias, EmployeeID string
	Calls, Failures   int
	Unpriced          int
	Tokens            int64
	Cost              float64
	Models            map[string]bool
	Live              bool
	Fingerprint       string
	GatewaySpend      string
}

func keyFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return "—"
	}
	if len(token) > 8 {
		return "hash ···" + token[len(token)-8:]
	}
	return "hash " + token
}

func keyModels(models []string) string {
	if len(models) == 0 {
		return "—"
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	if len(out) == 0 {
		return "—"
	}
	if len(out) > 4 {
		return strings.Join(out[:4], ", ") + fmt.Sprintf(" +%d", len(out)-4)
	}
	return strings.Join(out, ", ")
}

func (s *Server) loadKeyBillingPage(r *http.Request) (*keyBillingPage, error) {
	q := r.URL.Query()
	month, from, to := usageMonth(q.Get("month"), time.Now())
	page := &keyBillingPage{Month: month}
	values := url.Values{"month": {month}}
	page.Query = values.Encode()

	store := s.dbm.store.Usage()
	if first, last, err := store.Days(r.Context()); err == nil {
		page.Months = monthsBetween(first, last)
	} else if !errors.Is(err, repo.ErrNotFound) {
		return nil, fmt.Errorf("could not read the usage table: %w", err)
	}
	if !contains(page.Months, month) {
		page.Months = append([]string{month}, page.Months...)
	}

	rows, err := store.Rows(r.Context(), repo.UsageFilter{From: from, To: to}, csvLimit)
	if err != nil {
		return nil, fmt.Errorf("could not read the usage table: %w", err)
	}
	names := s.employeeNames(r)
	byAlias := map[string]*keyBillingAggregate{}
	get := func(alias string) *keyBillingAggregate {
		a := byAlias[alias]
		if a == nil {
			a = &keyBillingAggregate{Alias: alias, Models: map[string]bool{}}
			byAlias[alias] = a
		}
		return a
	}
	for _, row := range rows {
		a := get(row.KeyAlias)
		if a.EmployeeID == "" {
			a.EmployeeID = row.EmployeeID
		}
		a.Calls += row.Calls
		a.Failures += row.Failures
		a.Unpriced += row.Unpriced
		a.Tokens += row.PromptTokens + row.CompletionTokens
		cost, _ := strconv.ParseFloat(row.CostUSD, 64)
		a.Cost += cost
		if row.ModelGroup != "" {
			a.Models[row.ModelGroup] = true
		}
	}

	// A gateway outage must not hide the local, auditable snapshot. The live
	// columns simply remain unavailable and the page explains why.
	gatewayReady := false
	gw, gwErr := s.gateway()
	if gwErr != nil {
		page.GatewayError = gwErr.Error()
	} else {
		ctx, cancel := contextWithTimeout(r, gatewayTimeout)
		defer cancel()
		keys, err := gw.ListKeys(ctx)
		if err != nil {
			page.GatewayError = "无法读取 LiteLLM API Key 清单：" + err.Error()
		} else {
			gatewayReady = true
			employeesByGatewayID := map[string]repo.Employee{}
			for _, employee := range names {
				employeesByGatewayID[worker.GatewayUserID(employee.WindowsUser)] = employee
			}
			for _, key := range keys {
				if strings.TrimSpace(key.KeyAlias) == "" {
					continue
				}
				a := get(key.KeyAlias)
				a.Live = true
				a.Fingerprint = keyFingerprint(key.Token)
				a.GatewaySpend = money4(strconv.FormatFloat(key.Spend, 'f', 4, 64))
				for _, model := range key.Models {
					if model = strings.TrimSpace(model); model != "" {
						a.Models[model] = true
					}
				}
				if a.EmployeeID == "" {
					if employee, ok := employeesByGatewayID[key.UserID]; ok {
						a.EmployeeID = employee.ID
					}
				}
			}
		}
	}

	var localCost float64
	for _, a := range byAlias {
		localCost += a.Cost
		name, department := "", ""
		if employee, ok := names[a.EmployeeID]; ok {
			name, department = employee.WindowsUser, employee.Department
		}
		if name == "" && a.EmployeeID != "" {
			name = a.EmployeeID
		}
		status := "本地快照"
		if gatewayReady {
			status = "历史/已吊销"
		}
		if a.Live {
			status = "当前 Key"
		}
		models := make([]string, 0, len(a.Models))
		for model := range a.Models {
			models = append(models, model)
		}
		sort.Strings(models)
		page.Rows = append(page.Rows, keyBillingRow{
			Alias: a.Alias, Employee: name, Department: department,
			Fingerprint: a.Fingerprint, LocalCost: money4(strconv.FormatFloat(a.Cost, 'f', 4, 64)),
			GatewaySpend: a.GatewaySpend, Calls: a.Calls, Failures: a.Failures,
			Tokens: a.Tokens, Unpriced: a.Unpriced, Models: keyModels(models), Status: status, Live: a.Live,
		})
		page.Total.Calls += a.Calls
		page.Total.Failures += a.Failures
		page.Total.Unpriced += a.Unpriced
		page.Total.Tokens += a.Tokens
	}
	page.Total.Keys = len(page.Rows)
	page.Total.LocalCost = money4(strconv.FormatFloat(localCost, 'f', 4, 64))
	sort.Slice(page.Rows, func(i, j int) bool {
		left, _ := strconv.ParseFloat(page.Rows[i].LocalCost, 64)
		right, _ := strconv.ParseFloat(page.Rows[j].LocalCost, 64)
		if left != right {
			return left > right
		}
		return page.Rows[i].Alias < page.Rows[j].Alias
	})
	return page, nil
}

// contextWithTimeout is kept tiny so tests can use a regular request context;
// all LiteLLM reads remain bounded even when the gateway is unreachable.
func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}

func (s *Server) handleKeyBilling(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "usage")
	data.Tab = "keys"
	page, err := s.loadKeyBillingPage(r)
	if err != nil {
		data.Error = err.Error()
		month, _, _ := usageMonth(r.URL.Query().Get("month"), time.Now())
		page = &keyBillingPage{Month: month}
	}
	data.KeyBillingPage = page
	s.render(w, "key_billing.html", http.StatusOK, data)
}

func (s *Server) handleKeyBillingCSV(w http.ResponseWriter, r *http.Request, _ *session) {
	page, err := s.loadKeyBillingPage(r)
	if err != nil {
		http.Error(w, "导出失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="key-billing-`+page.Month+`.csv"`)
	w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"month", "key_alias", "employee", "department", "fingerprint", "local_cost_usd", "gateway_current_spend_usd", "calls", "failures", "tokens", "unpriced", "models", "status"})
	for _, row := range page.Rows {
		_ = cw.Write([]string{page.Month, csvSafe(row.Alias), csvSafe(row.Employee), csvSafe(row.Department), csvSafe(row.Fingerprint), row.LocalCost, row.GatewaySpend,
			strconv.Itoa(row.Calls), strconv.Itoa(row.Failures), strconv.FormatInt(row.Tokens, 10), strconv.Itoa(row.Unpriced), csvSafe(row.Models), csvSafe(row.Status)})
	}
	cw.Flush()
}
