package adminweb

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
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
	if !r.OnRoster {
		t.Errorf("a roster-derived row must be marked as such: %+v", r)
	}
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
	if rows[0].OnRoster {
		t.Error("a row invented from a token alias is not on the roster; its detail page would 404")
	}
}

func TestReconcileAccountsSortsByUser(t *testing.T) {
	rows := reconcileAccounts([]model.UserEntry{{WindowsUser: "zed"}, {WindowsUser: "amy"}}, nil, nil, nil)
	if rows[0].WindowsUser != "amy" || rows[1].WindowsUser != "zed" {
		t.Errorf("order: %v %v", rows[0].WindowsUser, rows[1].WindowsUser)
	}
}

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
		CodexAccount: "alice@codex.example", ClaudeAccount: "alice@claude.example",
		HasUser: true, HasToken: true, Models: []string{"glm-5.2"}, Spend: 17, Budget: 20,
		BudgetResetAt: "2026-10-01T00:00:00Z", Quota: litellm.Quota{MonthlyBudgetUSD: 20, RPM: 60, TPM: 200000, Parallel: 4},
		Machines: []string{"PC-1"}, OnRoster: true}
	flagged := accountRow{WindowsUser: "carol", Enabled: false, HasToken: true, OnRoster: true,
		Flags: []accountFlag{flagDepartedToken}}
	ghost := accountRow{WindowsUser: "ghost", Enabled: false, HasToken: true,
		Flags: []accountFlag{flagDepartedToken}}
	data := pageData{CSRF: "x", GatewayEnabled: true, GatewayURL: "https://gw.example",
		GatewayModels: []litellm.Model{{Name: "glm-5.2", Info: litellm.ModelInfo{DisplayName: "GLM"}}},
		Accounts:      []accountRow{row, flagged, ghost}, QuotaDefaults: admincore.DefaultQuota}

	var list strings.Builder
	if err := s.tpl.ExecuteTemplate(&list, "users.html", data); err != nil {
		t.Fatalf("users.html: %v", err)
	}
	for _, want := range []string{"Alice", "研发", "17.00", "20.00", "s-warn w85", "已离职仍有令牌", "/users/offboard", "/users/onboard", `href="/users/detail?user=alice"`} {
		if !strings.Contains(list.String(), want) {
			t.Errorf("users.html missing %q", want)
		}
	}
	// ghost has a token but no roster entry: /users/detail would 404 on it,
	// so the name must not be a link. The 修复 button stays -- offboarding
	// is what revokes the stray token.
	if strings.Contains(list.String(), `href="/users/detail?user=ghost"`) {
		t.Errorf("users.html links a row that has no roster entry to a page that 404s")
	}
	if n := strings.Count(list.String(), `action="/users/offboard"`); n < 3 {
		t.Errorf("users.html must keep the 修复 button on the stray-token row (%d offboard forms)", n)
	}

	// carol is departed with a lingering token (Fix "offboard"): her row must
	// carry an offboard form of her own, not just alice's still-employed one.
	if n := strings.Count(list.String(), `action="/users/offboard"`); n < 2 {
		t.Errorf("users.html has %d offboard forms, want at least 2 (alice's own, carol's fix)", n)
	}

	data.Account = &row
	data.Audit = []admincore.AuditEntry{{At: "2026-09-07T01:02:03Z", Action: admincore.AuditOnboard, User: "alice"}}
	var detail strings.Builder
	if err := s.tpl.ExecuteTemplate(&detail, "user.html", data); err != nil {
		t.Fatalf("user.html: %v", err)
	}
	for _, want := range []string{
		"/users/quota", "/users/models", "/users/reissue", "/users/offboard", "onboard", "PC-1", `value="60"`,
		"alice@codex.example", "alice@claude.example",
	} {
		if !strings.Contains(detail.String(), want) {
			t.Errorf("user.html missing %q", want)
		}
	}

	// The detail page for a departed-with-lingering-token account must also
	// offer the offboard fix, not just the "reopen" button.
	data.Account = &flagged
	var flaggedDetail strings.Builder
	if err := s.tpl.ExecuteTemplate(&flaggedDetail, "user.html", data); err != nil {
		t.Fatalf("user.html (flagged): %v", err)
	}
	if !strings.Contains(flaggedDetail.String(), `action="/users/offboard"`) {
		t.Errorf("user.html for a departed-with-token account is missing the offboard fix form")
	}

	// When the gateway is unreachable, offboarding must still work and
	// onboarding must not be offered as if it would.
	data.Account = nil
	data.GatewayEnabled = false
	var noGateway strings.Builder
	if err := s.tpl.ExecuteTemplate(&noGateway, "users.html", data); err != nil {
		t.Fatalf("users.html (no gateway): %v", err)
	}
	if !strings.Contains(noGateway.String(), `action="/users/offboard"`) {
		t.Errorf("users.html with no gateway lost the offboard form")
	}
	if !strings.Contains(noGateway.String(), `开户并下发配置</button>`) ||
		!strings.Contains(noGateway.String(), `<button type="submit" disabled>开户并下发配置</button>`) {
		t.Errorf("users.html with no gateway must disable the onboard submit button")
	}
}

func TestDropGatewayFlags(t *testing.T) {
	rows := []accountRow{
		{WindowsUser: "a", Flags: []accountFlag{flagNoToken}},
		{WindowsUser: "b", Flags: []accountFlag{flagNoUser}},
		{WindowsUser: "c", Flags: []accountFlag{flagDepartedToken}},
		{WindowsUser: "d", Flags: []accountFlag{flagDepartedBound}},
		{WindowsUser: "e", Flags: []accountFlag{flagDepartedToken, flagDepartedBound}},
		{WindowsUser: "f", Flags: nil},
	}
	got := dropGatewayFlags(rows)
	want := map[string][]accountFlag{
		"a": nil,
		"b": nil,
		"c": nil,
		"d": {flagDepartedBound},
		"e": {flagDepartedBound},
		"f": nil,
	}
	for _, r := range got {
		w := want[r.WindowsUser]
		if len(r.Flags) != len(w) {
			t.Errorf("%s: flags = %+v, want %+v", r.WindowsUser, r.Flags, w)
			continue
		}
		for i := range w {
			if r.Flags[i] != w[i] {
				t.Errorf("%s: flags = %+v, want %+v", r.WindowsUser, r.Flags, w)
				break
			}
		}
	}
}

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
