package adminweb

import (
	"net/http"
	"sort"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// customerDataPage is what a customer's manager may read beyond tickets.
type customerDataPage struct {
	CSRF, Name, View string
	Manager          bool
	Month            string
	Months           []string
	Total            customerUsageLine
	ByEmployee       []customerUsageLine
	ByModel          []customerUsageLine
	Employees        []repo.Employee
}

// customerUsageLine leaves out cost: usage_daily holds what the gateway paid,
// which is not what a customer is charged.
type customerUsageLine struct {
	Label, Sub      string
	Calls, Failures int
	Tokens          int64
}

func (l *customerUsageLine) add(r repo.UsageRow) {
	l.Calls += r.Calls
	l.Failures += r.Failures
	l.Tokens += r.PromptTokens + r.CompletionTokens
}

// customerManager lets a request through for a contact who sees the whole
// company: a manager, or a viewer, who may look but not approve.
func (s *Server) customerManager(w http.ResponseWriter, r *http.Request) (dbstore.CustomerContact, string, bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return dbstore.CustomerContact{}, "", false
	}
	c, csrf, ok := s.customerRequired(w, r)
	if !ok {
		return c, csrf, false
	}
	if !dbstore.SeesCompany(c.Role) {
		http.Error(w, "只有客戶管理員可以查看", http.StatusForbidden)
		return c, csrf, false
	}
	return c, csrf, true
}

// customerUsageLines keeps only the rows of this console's employees. The
// logstore is shared, so usage_daily also holds other companies' calls, under
// aliases that resolve to nobody here.
func customerUsageLines(rows []repo.UsageRow, employees map[string]repo.Employee) (total customerUsageLine, byEmployee, byModel []customerUsageLine) {
	people, models := map[string]*customerUsageLine{}, map[string]*customerUsageLine{}
	for _, row := range rows {
		e, ours := employees[row.EmployeeID]
		if !ours {
			continue
		}
		total.add(row)
		person := people[e.ID]
		if person == nil {
			person = &customerUsageLine{Label: e.Name, Sub: e.WindowsUser}
			if person.Label == "" {
				person.Label, person.Sub = e.WindowsUser, ""
			}
			people[e.ID] = person
		}
		person.add(row)
		model := models[row.ModelGroup]
		if model == nil {
			model = &customerUsageLine{Label: row.ModelGroup}
			models[row.ModelGroup] = model
		}
		model.add(row)
	}
	sorted := func(lines map[string]*customerUsageLine) []customerUsageLine {
		out := make([]customerUsageLine, 0, len(lines))
		for _, line := range lines {
			out = append(out, *line)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Tokens != out[j].Tokens {
				return out[i].Tokens > out[j].Tokens
			}
			return out[i].Label < out[j].Label
		})
		return out
	}
	return total, sorted(people), sorted(models)
}

func (s *Server) handleCustomerUsage(w http.ResponseWriter, r *http.Request) {
	c, csrf, ok := s.customerManager(w, r)
	if !ok {
		return
	}
	month, from, to := usageMonth(r.URL.Query().Get("month"), time.Now())
	rows, err := s.dbm.store.Usage().Rows(r.Context(), repo.UsageFilter{From: from, To: to}, 0)
	if err != nil {
		http.Error(w, "無法讀取用量", http.StatusInternalServerError)
		return
	}
	page := customerDataPage{CSRF: csrf, Name: c.Name, View: "usage", Manager: true, Month: month}
	page.Total, page.ByEmployee, page.ByModel = customerUsageLines(rows, s.employeeNames(r))
	if first, last, err := s.dbm.store.Usage().Days(r.Context()); err == nil && !first.IsZero() {
		page.Months = monthsBetween(first, last)
	}
	s.tpl.ExecuteTemplate(w, "customer_data.html", page)
}

func (s *Server) handleCustomerEmployees(w http.ResponseWriter, r *http.Request) {
	c, csrf, ok := s.customerManager(w, r)
	if !ok {
		return
	}
	employees, err := s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		http.Error(w, "無法讀取員工名單", http.StatusInternalServerError)
		return
	}
	s.tpl.ExecuteTemplate(w, "customer_data.html", customerDataPage{
		CSRF: csrf, Name: c.Name, View: "employees", Manager: true, Employees: dailyVisibleEmployees(employees),
	})
}
