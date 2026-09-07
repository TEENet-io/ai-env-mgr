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
