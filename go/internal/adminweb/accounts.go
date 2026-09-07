package adminweb

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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

	// OnRoster is false for a row invented from a gateway token whose alias
	// matches nobody. There is no roster entry behind it, so /users/detail
	// would 404: the list renders the name as plain text, not a link.
	OnRoster bool

	// Administrator notes, carried through from the roster untouched by
	// anything here -- see admincore.AccountSpec.
	CodexAccount  string
	ClaudeAccount string

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

	build := func(e model.UserEntry, onRoster bool) accountRow {
		id := admincore.KeyAlias(e.WindowsUser)
		r := accountRow{
			WindowsUser: e.WindowsUser, Name: e.Name, Department: e.Department, Enabled: e.Enabled,
			OnRoster:     onRoster,
			CodexAccount: e.CodexAccount, ClaudeAccount: e.ClaudeAccount,
		}
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
		out = append(out, build(e, true))
	}
	// Employee tokens whose alias matches nobody on the roster. No roster
	// row would ever show them, and they keep working until revoked.
	for alias := range keyByAlias {
		if claimed[alias] || !strings.HasPrefix(alias, "emp-") {
			continue
		}
		out = append(out, build(model.UserEntry{WindowsUser: strings.TrimPrefix(alias, "emp-"), Enabled: false}, false))
	}

	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].WindowsUser) < strings.ToLower(out[j].WindowsUser)
	})
	return out
}

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

// dropGatewayFlags strips the flags that reconcileAccounts derives from
// gateway state (flagNoToken, flagNoUser, flagDepartedToken) from every row,
// keeping only flagDepartedBound.
//
// When the gateway is unreachable, ListKeys/ListUsers/FindKeyByAlias/UserInfo
// all come back nil rather than "we asked and there truly is nothing there",
// so reconcileAccounts' "no token"/"no user" flags are an artifact of the
// outage, not a fact about the account -- showing them would tell an
// operator to fix something that may already be fine. flagDepartedBound
// comes from machine bindings, which do not depend on the gateway, so it
// stays.
func dropGatewayFlags(rows []accountRow) []accountRow {
	for i := range rows {
		var kept []accountFlag
		for _, f := range rows[i].Flags {
			if f == flagDepartedBound {
				kept = append(kept, f)
			}
		}
		rows[i].Flags = kept
	}
	return rows
}

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
	if !data.GatewayEnabled {
		data.Accounts = dropGatewayFlags(data.Accounts)
	}
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
	if !data.GatewayEnabled {
		rows = dropGatewayFlags(rows)
	}
	data.Account = &rows[0]
	s.render(w, "user.html", http.StatusOK, data)
}

// parseQuotaForm reads the four quota fields. All are required: see
// litellm.Quota for why zero cannot mean unlimited.
func parseQuotaForm(form url.Values) (litellm.Quota, error) {
	var q litellm.Quota
	budget := strings.TrimSpace(form.Get("budget"))
	if budget == "" {
		return q, fmt.Errorf("月预算不能为空")
	}
	var err error
	if q.MonthlyBudgetUSD, err = strconv.ParseFloat(budget, 64); err != nil {
		return q, fmt.Errorf("月预算需要是一个数字")
	}
	// A slice, not a map, so the first reported error is deterministic
	// rather than depending on map iteration order.
	ints := []struct {
		name  string
		label string
		dst   *int
	}{
		{"rpm", "每分钟请求数", &q.RPM},
		{"tpm", "每分钟 token 数", &q.TPM},
		{"parallel", "并发数", &q.Parallel},
	}
	for _, f := range ints {
		raw := strings.TrimSpace(form.Get(f.name))
		if raw == "" {
			return q, fmt.Errorf("%s不能为空", f.label)
		}
		if *f.dst, err = strconv.Atoi(raw); err != nil {
			return q, fmt.Errorf("%s需要是一个整数", f.label)
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
		WindowsUser:   user,
		Name:          formValue(r, "name"),
		Department:    formValue(r, "department"),
		Quota:         quota,
		Models:        r.PostForm["models"], // none selected = everything the gateway offers
		CodexAccount:  formValue(r, "codexAccount"),
		ClaudeAccount: formValue(r, "claudeAccount"),
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
	defaults, err := sess.mgr.LoadQuotaDefaults()
	if err != nil {
		return err
	}
	alias := admincore.KeyAlias(e.WindowsUser)
	u, userFound, err := gw.UserInfo(ctx, alias)
	if err != nil {
		return fmt.Errorf("look up gateway user: %w", err)
	}
	var k litellm.Key
	var keyFound bool
	if !userFound {
		if k, keyFound, err = gw.FindKeyByAlias(ctx, alias); err != nil {
			return fmt.Errorf("look up gateway token: %w", err)
		}
	}
	return sess.mgr.Onboard(ctx, gw, cfg, reopenSpec(*e, defaults, u, userFound, k, keyFound))
}

// reopenSpec decides what a row-button reopen re-onboards with: the labels
// from the roster, the limits and allowlist from whatever the gateway
// already holds.
//
// The key's allowlist is the fallback when there is a token but no user
// record -- the pre-user state the list flags as 有令牌无网关用户. Without
// it that reopen would pass no allowlist at all, which means "every model
// the gateway offers": a repair button that quietly widens what somebody may
// use. Limits stored as something Quota.Validate rejects (a user created
// outside this console, say) fall back to the defaults rather than being
// mirrored back.
func reopenSpec(e model.UserEntry, defaults litellm.Quota, u litellm.User, userFound bool, k litellm.Key, keyFound bool) admincore.AccountSpec {
	spec := admincore.AccountSpec{
		WindowsUser: e.WindowsUser, Name: e.Name, Department: e.Department, Quota: defaults,
	}
	switch {
	case userFound:
		if q := u.Quota(); q.Validate() == nil {
			spec.Quota = q
		}
		spec.Models = u.Models
	case keyFound:
		spec.Models = k.Models
	}
	return spec
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

// actionAccountProfile saves the detail page's 基本信息 form. Every field is
// posted, filled in from the roster, so an emptied one is an edit and is
// written through as such (admincore.UpdateProfile).
func (s *Server) actionAccountProfile(sess *session, r *http.Request) error {
	gw, _, ctx, cancel, err := s.accountContext(r)
	if err != nil {
		return err
	}
	defer cancel()
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}
	return sess.mgr.UpdateProfile(ctx, gw, user,
		formValue(r, "name"), formValue(r, "department"),
		formValue(r, "codexAccount"), formValue(r, "claudeAccount"))
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
