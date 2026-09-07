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
