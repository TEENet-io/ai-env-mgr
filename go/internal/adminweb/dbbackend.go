package adminweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/status"
	"github.com/TEENet-io/ai-env-mgr/internal/worker"
)

// dbBackend is the console over the database.
//
// Reads come from the tables; writes go through ops, which records the
// decision and queues the work. OSS is touched directly only for what still
// lives there by design -- machine logs, collected session data and the
// binaries a publish uploads -- using the server's own identity, never the
// administrator's.
type dbBackend struct {
	store   repo.Store
	ops     *ops.Service
	objects admincore.Store
	// actor and requestID go on every audit row this session writes.
	actor     string
	requestID string
}

func (b dbBackend) Roster(ctx context.Context) ([]model.UserEntry, error) {
	employees, err := b.store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		return nil, err
	}
	out := make([]model.UserEntry, 0, len(employees))
	for _, e := range employees {
		out = append(out, entryFrom(e))
	}
	return out, nil
}

func entryFrom(e repo.Employee) model.UserEntry {
	return model.UserEntry{
		WindowsUser: e.WindowsUser, Name: e.Name, Department: e.Department,
		CodexAccount: e.CodexAccount, Enabled: e.Active(),
	}
}

func (b dbBackend) Policy(ctx context.Context) (model.Policy, error) {
	p, _, err := b.ops.CurrentPolicy(ctx)
	return p, err
}

// Machines assembles the fleet view from the tables: every machine that has
// reported or been bound, with its newest report and its current holder.
func (b dbBackend) Machines(ctx context.Context) ([]admincore.MachineState, error) {
	devices, err := b.store.Devices().List(ctx, repo.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	reports, err := b.store.Reports().List(ctx)
	if err != nil {
		return nil, err
	}
	reportOf := make(map[string]repo.DeviceReport, len(reports))
	for _, r := range reports {
		reportOf[r.DeviceID] = r
	}
	open, err := b.store.Bindings().ListOpen(ctx)
	if err != nil {
		return nil, err
	}
	bindingOf := make(map[string]repo.Binding, len(open))
	for _, bd := range open {
		bindingOf[bd.DeviceID] = bd
	}
	employees, err := b.store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		return nil, err
	}
	employeeOf := make(map[string]repo.Employee, len(employees))
	for _, e := range employees {
		employeeOf[e.ID] = e
	}

	states := make([]admincore.MachineState, 0, len(devices))
	for _, d := range devices {
		st := model.Status{Machine: d.Hostname}
		hasStatus := false
		if r, ok := reportOf[d.ID]; ok {
			// The report keeps the whole object, so the console reads exactly
			// what the agent wrote rather than a projection of it.
			if parsed, err := status.Parse(r.Report); err == nil {
				st, hasStatus = parsed, true
			}
		}
		var binding model.Binding
		bound, disabled := false, false
		if bd, ok := bindingOf[d.ID]; ok {
			if e, ok := employeeOf[bd.EmployeeID]; ok {
				bound = true
				disabled = !e.Active()
				binding = bindingFrom(bd, e)
			}
		}
		states = append(states, admincore.AssembleMachineState(
			d.Hostname, st, hasStatus, binding, bound, disabled, freshAfter))
	}
	sort.Slice(states, func(i, j int) bool {
		return strings.ToLower(states[i].Machine) < strings.ToLower(states[j].Machine)
	})
	return states, nil
}

func bindingFrom(bd repo.Binding, e repo.Employee) model.Binding {
	out := model.Binding{
		User:    e.WindowsUser,
		BoundAt: bd.BoundAt.UTC().Format(time.RFC3339),
		Note:    bd.Note,
	}
	if bd.RestartNonce != "" {
		out.RestartCodex = bd.RestartNonce
		if bd.RestartAt != nil {
			out.RestartCodexAt = bd.RestartAt.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func (b dbBackend) Bindings(ctx context.Context) (map[string]model.Binding, error) {
	open, err := b.store.Bindings().ListOpen(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]model.Binding, len(open))
	for _, bd := range open {
		device, err := b.store.Devices().ByID(ctx, bd.DeviceID)
		if err != nil {
			return nil, err
		}
		employee, err := b.store.Employees().ByID(ctx, bd.EmployeeID)
		if err != nil {
			return nil, err
		}
		out[device.Hostname] = bindingFrom(bd, employee)
	}
	return out, nil
}

func (b dbBackend) QuotaDefaults(ctx context.Context) (litellm.Quota, error) {
	q, _, err := b.ops.QuotaDefaults(ctx)
	if err != nil {
		return litellm.Quota{}, err
	}
	return gatewayQuota(q)
}

// gatewayQuota converts the stored decimal to the float the gateway's API and
// the forms speak. The conversion happens at this boundary and nowhere else.
func gatewayQuota(q repo.Quota) (litellm.Quota, error) {
	budget, err := strconv.ParseFloat(q.MonthlyBudget, 64)
	if err != nil {
		return litellm.Quota{}, fmt.Errorf("stored budget %q is not a number", q.MonthlyBudget)
	}
	return litellm.Quota{MonthlyBudgetUSD: budget, RPM: q.RPM, TPM: q.TPM, Parallel: q.Parallel}, nil
}

func storedQuota(q litellm.Quota) repo.Quota {
	return repo.Quota{
		MonthlyBudget: strconv.FormatFloat(q.MonthlyBudgetUSD, 'f', -1, 64),
		RPM:           q.RPM, TPM: q.TPM, Parallel: q.Parallel,
	}
}

func (b dbBackend) CollectStats(ctx context.Context) ([]admincore.CollectStat, bool, error) {
	roster, err := b.Roster(ctx)
	if err != nil {
		return nil, false, err
	}
	users := make([]string, 0, len(roster))
	for _, e := range roster {
		users = append(users, e.WindowsUser)
	}
	stats, err := admincore.CollectStatsFor(b.objects, users)
	if err != nil {
		return nil, false, err
	}
	p, err := b.Policy(ctx)
	if err != nil {
		return nil, false, err
	}
	return stats, p.CollectEnabled, nil
}

// Audit renders the employee's history in the shape the page already draws:
// time, action, and a flat detail map assembled from before, after and detail.
func (b dbBackend) Audit(ctx context.Context, windowsUser string) ([]admincore.AuditEntry, error) {
	employee, err := b.store.Employees().ByWindowsUser(ctx, windowsUser)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	events, err := b.store.Audit().ByTarget(ctx, "employee", employee.ID, 200)
	if err != nil {
		return nil, err
	}
	out := make([]admincore.AuditEntry, 0, len(events))
	for _, ev := range events {
		detail := map[string]any{}
		if ev.ActorID != "" {
			detail["by"] = ev.ActorID
		}
		mergeJSON(detail, "before", ev.Before)
		mergeJSON(detail, "after", ev.After)
		mergeJSON(detail, "", ev.Detail)
		out = append(out, admincore.AuditEntry{
			At:     ev.OccurredAt.UTC().Format(time.RFC3339),
			Action: admincore.AuditAction(ev.Action),
			User:   windowsUser,
			Detail: detail,
		})
	}
	return out, nil
}

// mergeJSON flattens one JSON object into the detail map, prefixing its keys.
// Non-objects are kept whole under the prefix.
func mergeJSON(into map[string]any, prefix string, raw []byte) {
	if len(raw) == 0 {
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		if prefix != "" {
			into[prefix] = v
		}
		return
	}
	for k, val := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		// Nested objects render as JSON: a row is read, not parsed.
		if nested, ok := val.(map[string]any); ok {
			if encoded, err := json.Marshal(nested); err == nil {
				val = string(encoded)
			}
		}
		into[key] = val
	}
}

func (b dbBackend) FetchLog(_ context.Context, machine string) ([]byte, error) {
	data, _, err := b.objects.Get(ossclient.LogKey(machine))
	return data, err
}

// AccountRows is the account list from the tables, with spend from the
// gateway when it answered. HasToken means the database holds a live grant
// that the gateway was last seen to honour; the gateway's own key list is not
// consulted, because the grant table is what says which token is whose.
func (b dbBackend) AccountRows(ctx context.Context, _ []litellm.Key, gwUsers []litellm.User) ([]accountRow, error) {
	employees, err := b.store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		return nil, err
	}
	rows := make([]accountRow, 0, len(employees))
	for _, e := range employees {
		row, err := b.accountRow(ctx, e, gwUsers)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].WindowsUser) < strings.ToLower(rows[j].WindowsUser)
	})
	return rows, nil
}

func (b dbBackend) AccountRow(ctx context.Context, windowsUser string, _ []litellm.Key, gwUsers []litellm.User) (*accountRow, error) {
	employee, err := b.store.Employees().ByWindowsUser(ctx, windowsUser)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row, err := b.accountRow(ctx, employee, gwUsers)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (b dbBackend) accountRow(ctx context.Context, e repo.Employee, gwUsers []litellm.User) (accountRow, error) {
	row := accountRow{
		WindowsUser: e.WindowsUser, Name: e.Name, Department: e.Department, Enabled: e.Active(),
		OnRoster: true, CodexAccount: e.CodexAccount, Version: e.Version,
	}
	if q, err := b.store.Quotas().Get(ctx, e.ID); err == nil {
		row.QuotaVersion = q.Version
		if gq, err := gatewayQuota(q); err == nil {
			row.Quota = gq
			row.Budget = gq.MonthlyBudgetUSD
		}
	} else if !errors.Is(err, repo.ErrNotFound) {
		return accountRow{}, err
	}
	models, err := b.store.Employees().Models(ctx, e.ID)
	if err != nil {
		return accountRow{}, err
	}
	row.Models = models

	grant, err := b.store.Grants().Active(ctx, "", e.ID)
	switch {
	case err == nil:
		row.HasToken = grant.Actual == repo.ActualActive || grant.Actual == repo.ActualUnknown
	case !errors.Is(err, repo.ErrNotFound):
		return accountRow{}, err
	}

	userID := worker.GatewayUserID(e.WindowsUser)
	for _, u := range gwUsers {
		if u.UserID == userID {
			row.HasUser = true
			row.Spend = u.Spend
			row.BudgetResetAt = u.BudgetResetAt
			break
		}
	}

	bindings, err := b.store.Bindings().OpenByEmployee(ctx, e.ID)
	if err != nil {
		return accountRow{}, err
	}
	for _, bd := range bindings {
		device, err := b.store.Devices().ByID(ctx, bd.DeviceID)
		if err != nil {
			return accountRow{}, err
		}
		row.Machines = append(row.Machines, device.Hostname)
	}
	sort.Strings(row.Machines)

	switch {
	case row.Enabled && !row.HasToken:
		row.Flags = append(row.Flags, flagNoToken)
	case row.Enabled && row.HasToken && !row.HasUser && len(gwUsers) > 0:
		row.Flags = append(row.Flags, flagNoUser)
	}
	if !row.Enabled && row.HasToken {
		row.Flags = append(row.Flags, flagDepartedToken)
	}
	if !row.Enabled && len(row.Machines) > 0 {
		row.Flags = append(row.Flags, flagDepartedBound)
	}
	return row, nil
}

// --- actions ---

func (b dbBackend) Onboard(ctx context.Context, _ *litellm.Client, _ admincore.GatewayConfig, spec admincore.AccountSpec) error {
	_, err := b.ops.Onboard(ctx, ops.OnboardSpec{
		WindowsUser: spec.WindowsUser, Name: spec.Name, Department: spec.Department,
		CodexAccount: spec.CodexAccount,
		Quota:        storedQuota(spec.Quota), Models: spec.Models,
		Actor: b.actor, RequestID: b.requestID,
	})
	return err
}

func (b dbBackend) employee(ctx context.Context, windowsUser string) (repo.Employee, error) {
	e, err := b.store.Employees().ByWindowsUser(ctx, windowsUser)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Employee{}, fmt.Errorf("user %q not found in roster", windowsUser)
	}
	return e, err
}

func (b dbBackend) Offboard(ctx context.Context, _ *litellm.Client, windowsUser string) error {
	e, err := b.employee(ctx, windowsUser)
	if err != nil {
		return err
	}
	_, err = b.ops.Offboard(ctx, e.ID, b.actor, b.requestID)
	return err
}

func (b dbBackend) DeleteAccount(ctx context.Context, windowsUser string) error {
	e, err := b.employee(ctx, windowsUser)
	if err != nil {
		return err
	}
	_, err = b.ops.Delete(ctx, e.ID, b.actor, b.requestID)
	return err
}

// DeletedAccounts lists removed accounts as rows with no actions and no
// link: there is no detail page behind a deleted account.
func (b dbBackend) DeletedAccounts(ctx context.Context) ([]accountRow, error) {
	employees, err := b.store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true, IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	rows := []accountRow{}
	for _, e := range employees {
		if !e.Deleted() {
			continue
		}
		rows = append(rows, accountRow{
			WindowsUser: e.WindowsUser, Name: e.Name, Department: e.Department,
			CodexAccount: e.CodexAccount, Deleted: true, DeletedAt: e.DeletedAt.Format("2006-01-02"),
		})
	}
	return rows, nil
}

func (b dbBackend) SetQuota(ctx context.Context, _ *litellm.Client, windowsUser string, q litellm.Quota, version int) error {
	e, err := b.employee(ctx, windowsUser)
	if err != nil {
		return err
	}
	if version == 0 {
		// A form that carried no version (the list page's quick actions)
		// saves against whatever is current.
		if current, err := b.store.Quotas().Get(ctx, e.ID); err == nil {
			version = current.Version
		} else if !errors.Is(err, repo.ErrNotFound) {
			return err
		}
	}
	return b.ops.SetQuota(ctx, e.ID, storedQuota(q), version, b.actor, b.requestID)
}

func (b dbBackend) SetModels(ctx context.Context, _ *litellm.Client, _ admincore.GatewayConfig, windowsUser string, models []string) error {
	e, err := b.employee(ctx, windowsUser)
	if err != nil {
		return err
	}
	return b.ops.SetModels(ctx, e.ID, models, b.actor, b.requestID)
}

func (b dbBackend) UpdateProfile(ctx context.Context, _ *litellm.Client, windowsUser, name, department, codexAccount string, version int) error {
	e, err := b.employee(ctx, windowsUser)
	if err != nil {
		return err
	}
	if version == 0 {
		version = e.Version
	}
	return b.ops.UpdateProfile(ctx, e.ID, version, repo.Profile{
		Name: name, Department: department, CodexAccount: codexAccount,
		ExternalID: e.ExternalID,
	}, b.actor, b.requestID)
}

func (b dbBackend) Reissue(ctx context.Context, _ *litellm.Client, _ admincore.GatewayConfig, windowsUser string) error {
	e, err := b.employee(ctx, windowsUser)
	if err != nil {
		return err
	}
	_, err = b.ops.Reissue(ctx, e.ID, b.actor, b.requestID)
	return err
}

func (b dbBackend) BindMachine(ctx context.Context, machine, windowsUser, note string) error {
	e, err := b.employee(ctx, windowsUser)
	if err != nil {
		return err
	}
	_, err = b.ops.BindMachine(ctx, machine, e.ID, note, b.actor, b.requestID)
	return err
}

func (b dbBackend) UnbindMachine(ctx context.Context, machine string) error {
	return b.ops.UnbindMachine(ctx, machine, b.actor, b.requestID)
}

func (b dbBackend) ForgetMachine(ctx context.Context, machine string) error {
	return b.ops.ForgetMachine(ctx, machine, b.actor, b.requestID)
}

func (b dbBackend) RequestCodexRestart(ctx context.Context, machine string) error {
	_, err := b.ops.RequestCodexRestart(ctx, machine, b.actor, b.requestID)
	return err
}

func (b dbBackend) RequestSync(ctx context.Context, machine string) error {
	_, err := b.ops.RequestSync(ctx, machine, b.actor, b.requestID)
	return err
}

func (b dbBackend) MutateDomains(ctx context.Context, add, remove []string) error {
	_, err := b.ops.MutateDomains(ctx, add, remove, b.actor, b.requestID)
	return err
}

func (b dbBackend) MutateAppLockerAllowPaths(ctx context.Context, add, remove []string) error {
	_, err := b.ops.MutateAppLockerAllowPaths(ctx, add, remove, b.actor, b.requestID)
	return err
}

func (b dbBackend) SetAppLockerMode(ctx context.Context, mode string) error {
	_, err := b.ops.SetAppLockerMode(ctx, mode, b.actor, b.requestID)
	return err
}

func (b dbBackend) SetBlockEnabled(ctx context.Context, enabled bool) error {
	_, err := b.ops.SetBlockEnabled(ctx, enabled, b.actor, b.requestID)
	return err
}

func (b dbBackend) SetSyncInterval(ctx context.Context, minutes int) error {
	_, err := b.ops.SetSyncInterval(ctx, minutes, b.actor, b.requestID)
	return err
}

func (b dbBackend) SetCollect(ctx context.Context, enabled bool, since *string, quiet *int) error {
	_, err := b.ops.SetCollect(ctx, enabled, since, quiet, b.actor, b.requestID)
	return err
}

func (b dbBackend) SaveQuotaDefaults(ctx context.Context, q litellm.Quota) error {
	_, version, err := b.ops.QuotaDefaults(ctx)
	if err != nil {
		return err
	}
	return b.ops.SetQuotaDefaults(ctx, storedQuota(q), version, b.actor, b.requestID)
}

// PublishAgentUpdate is the legacy whole-package path. In the database mode
// packages come from CI through the bucket and are registered in the version
// library (/releases); nothing holds a package in memory. This exists only to
// satisfy the backend interface and refuses.
func (b dbBackend) PublishAgentUpdate(context.Context, string, []byte, func(done, total int64)) (string, error) {
	return "", errNoLegacyPublish
}

var errNoLegacyPublish = fmt.Errorf("publishing goes through the version library (/releases) in the database mode")

func (b dbBackend) CancelAgentUpdate(ctx context.Context) error {
	_, err := b.ops.SetAgentUpdate(ctx, "", "", b.actor, b.requestID)
	return err
}

func (b dbBackend) PublishCodexUpdate(context.Context, string, []byte, func(done, total int64)) (string, error) {
	return "", errNoLegacyPublish
}

func (b dbBackend) CancelCodexUpdate(ctx context.Context) error {
	_, err := b.ops.SetCodexUpdate(ctx, "", "", "", b.actor, b.requestID)
	return err
}
