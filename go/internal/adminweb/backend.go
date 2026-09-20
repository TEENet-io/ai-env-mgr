package adminweb

import (
	"context"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// backend is everything a page needs from the console's state, as an
// interface with two implementations: the OSS-backed Manager the console has
// always used, and the database.
//
// The handlers talk only to this. The switch from objects to tables is then a
// change of which implementation a session is given, not a rewrite of every
// page -- and while both exist, the same templates render both, so nothing
// can look right on one and wrong on the other without a test noticing.
//
// The legacy implementation goes away at cutover, together with sign-in by
// AccessKey.
type backend interface {
	Roster(ctx context.Context) ([]model.UserEntry, error)
	Policy(ctx context.Context) (model.Policy, error)
	Machines(ctx context.Context) ([]admincore.MachineState, error)
	Bindings(ctx context.Context) (map[string]model.Binding, error)
	QuotaDefaults(ctx context.Context) (litellm.Quota, error)
	CollectStats(ctx context.Context) ([]admincore.CollectStat, bool, error)
	Audit(ctx context.Context, windowsUser string) ([]admincore.AuditEntry, error)
	FetchLog(ctx context.Context, machine string) ([]byte, error)

	// AccountRows joins the roster with what the gateway reported. keys and
	// gwUsers may be nil when the gateway did not answer; the rows then carry
	// what this side knows.
	AccountRows(ctx context.Context, keys []litellm.Key, gwUsers []litellm.User) ([]accountRow, error)
	AccountRow(ctx context.Context, windowsUser string, keys []litellm.Key, gwUsers []litellm.User) (*accountRow, error)

	Onboard(ctx context.Context, gw *litellm.Client, cfg admincore.GatewayConfig, spec admincore.AccountSpec) error
	Offboard(ctx context.Context, gw *litellm.Client, windowsUser string) error
	// DeleteAccount removes a closed account from the console (database mode
	// only); DeletedAccounts lists what has been removed, for the one list
	// filter that shows them.
	DeleteAccount(ctx context.Context, windowsUser string) error
	DeletedAccounts(ctx context.Context) ([]accountRow, error)
	SetQuota(ctx context.Context, gw *litellm.Client, windowsUser string, q litellm.Quota) error
	SetModels(ctx context.Context, gw *litellm.Client, cfg admincore.GatewayConfig, windowsUser string, models []string) error
	UpdateProfile(ctx context.Context, gw *litellm.Client, windowsUser, name, department, codexAccount string) error
	Reissue(ctx context.Context, gw *litellm.Client, cfg admincore.GatewayConfig, windowsUser string) error

	BindMachine(ctx context.Context, machine, windowsUser, note string) error
	UnbindMachine(ctx context.Context, machine string) error
	ForgetMachine(ctx context.Context, machine string) error
	RequestCodexRestart(ctx context.Context, machine string) error
	// RequestSync asks the machine to sync now (database mode only).
	RequestSync(ctx context.Context, machine string) error

	MutateDomains(ctx context.Context, add, remove []string) error
	MutateAppLockerAllowPaths(ctx context.Context, add, remove []string) error
	SetAppLockerMode(ctx context.Context, mode string) error
	SetBlockEnabled(ctx context.Context, enabled bool) error
	SetSyncInterval(ctx context.Context, minutes int) error
	SetCollect(ctx context.Context, enabled bool, since *string, quiet *int) error
	SaveQuotaDefaults(ctx context.Context, q litellm.Quota) error

	PublishAgentUpdate(ctx context.Context, version string, binary []byte, onProgress func(done, total int64)) (string, error)
	CancelAgentUpdate(ctx context.Context) error
	PublishCodexUpdate(ctx context.Context, version string, installer []byte, onProgress func(done, total int64)) (string, error)
	CancelCodexUpdate(ctx context.Context) error
}

// legacyBackend is the OSS-backed Manager behind the interface. It exists so
// the console keeps working exactly as it did until the cutover; nothing here
// has any logic of its own.
type legacyBackend struct{ mgr *admincore.Manager }

func (b legacyBackend) Roster(context.Context) ([]model.UserEntry, error) {
	us, err := b.mgr.LoadUsers()
	if err != nil {
		return nil, err
	}
	return us.Users, nil
}

func (b legacyBackend) Policy(context.Context) (model.Policy, error) { return b.mgr.CurrentPolicy() }

func (b legacyBackend) Machines(context.Context) ([]admincore.MachineState, error) {
	return b.mgr.CollectMachines(freshAfter)
}

func (b legacyBackend) Bindings(context.Context) (map[string]model.Binding, error) {
	return b.mgr.ListBindings()
}

func (b legacyBackend) QuotaDefaults(context.Context) (litellm.Quota, error) {
	return b.mgr.LoadQuotaDefaults()
}

func (b legacyBackend) CollectStats(context.Context) ([]admincore.CollectStat, bool, error) {
	return b.mgr.CollectStats()
}

func (b legacyBackend) Audit(_ context.Context, windowsUser string) ([]admincore.AuditEntry, error) {
	return b.mgr.ReadAudit(windowsUser)
}

func (b legacyBackend) FetchLog(_ context.Context, machine string) ([]byte, error) {
	return b.mgr.FetchLog(machine)
}

func (b legacyBackend) AccountRows(_ context.Context, keys []litellm.Key, gwUsers []litellm.User) ([]accountRow, error) {
	us, err := b.mgr.LoadUsers()
	if err != nil {
		return nil, err
	}
	bindings, err := b.mgr.ListBindings()
	if err != nil {
		bindings = nil
	}
	return reconcileAccounts(us.Users, keys, gwUsers, bindings), nil
}

func (b legacyBackend) AccountRow(_ context.Context, windowsUser string, keys []litellm.Key, gwUsers []litellm.User) (*accountRow, error) {
	us, err := b.mgr.LoadUsers()
	if err != nil {
		return nil, err
	}
	e := us.Find(windowsUser)
	if e == nil {
		return nil, nil
	}
	bindings, _ := b.mgr.ListBindings()
	rows := reconcileAccounts([]model.UserEntry{*e}, keys, gwUsers, bindings)
	return &rows[0], nil
}

func (b legacyBackend) Onboard(ctx context.Context, gw *litellm.Client, cfg admincore.GatewayConfig, spec admincore.AccountSpec) error {
	return b.mgr.Onboard(ctx, gw, cfg, spec)
}

func (b legacyBackend) Offboard(ctx context.Context, gw *litellm.Client, windowsUser string) error {
	return b.mgr.Offboard(ctx, gw, windowsUser)
}

func (b legacyBackend) DeleteAccount(context.Context, string) error {
	return fmt.Errorf("deleting an account needs the database mode; the object store keeps the roster as one file")
}

func (b legacyBackend) DeletedAccounts(context.Context) ([]accountRow, error) { return nil, nil }

func (b legacyBackend) SetQuota(ctx context.Context, gw *litellm.Client, windowsUser string, q litellm.Quota) error {
	return b.mgr.SetQuota(ctx, gw, windowsUser, q)
}

func (b legacyBackend) SetModels(ctx context.Context, gw *litellm.Client, cfg admincore.GatewayConfig, windowsUser string, models []string) error {
	return b.mgr.SetModels(ctx, gw, cfg, windowsUser, models)
}

func (b legacyBackend) UpdateProfile(ctx context.Context, gw *litellm.Client, windowsUser, name, department, codexAccount string) error {
	return b.mgr.UpdateProfile(ctx, gw, windowsUser, name, department, codexAccount)
}

func (b legacyBackend) Reissue(ctx context.Context, gw *litellm.Client, cfg admincore.GatewayConfig, windowsUser string) error {
	return b.mgr.Reissue(ctx, gw, cfg, windowsUser)
}

func (b legacyBackend) BindMachine(_ context.Context, machine, windowsUser, note string) error {
	return b.mgr.BindMachine(machine, windowsUser, note)
}

func (b legacyBackend) UnbindMachine(_ context.Context, machine string) error {
	return b.mgr.UnbindMachine(machine)
}

func (b legacyBackend) ForgetMachine(_ context.Context, machine string) error {
	return b.mgr.ForgetMachine(machine)
}

func (b legacyBackend) RequestCodexRestart(_ context.Context, machine string) error {
	return b.mgr.RequestCodexRestart(machine)
}

func (b legacyBackend) RequestSync(context.Context, string) error {
	return fmt.Errorf("sync now needs the database mode")
}

func (b legacyBackend) MutateDomains(_ context.Context, add, remove []string) error {
	_, err := b.mgr.MutateDomains(add, remove)
	return err
}

func (b legacyBackend) MutateAppLockerAllowPaths(_ context.Context, add, remove []string) error {
	_, err := b.mgr.MutateAppLockerAllowPaths(add, remove)
	return err
}

func (b legacyBackend) SetAppLockerMode(_ context.Context, mode string) error {
	_, err := b.mgr.SetAppLockerMode(mode)
	return err
}

func (b legacyBackend) SetBlockEnabled(_ context.Context, enabled bool) error {
	_, err := b.mgr.SetBlockEnabled(enabled)
	return err
}

func (b legacyBackend) SetSyncInterval(_ context.Context, minutes int) error {
	_, err := b.mgr.SetSyncInterval(minutes)
	return err
}

func (b legacyBackend) SetCollect(_ context.Context, enabled bool, since *string, quiet *int) error {
	_, err := b.mgr.SetCollect(enabled, since, quiet)
	return err
}

func (b legacyBackend) SaveQuotaDefaults(_ context.Context, q litellm.Quota) error {
	return b.mgr.SaveQuotaDefaults(q)
}

func (b legacyBackend) PublishAgentUpdate(_ context.Context, version string, binary []byte, onProgress func(done, total int64)) (string, error) {
	return b.mgr.PublishAgentUpdate(version, binary, onProgress)
}

func (b legacyBackend) CancelAgentUpdate(context.Context) error { return b.mgr.CancelAgentUpdate() }

func (b legacyBackend) PublishCodexUpdate(_ context.Context, version string, installer []byte, onProgress func(done, total int64)) (string, error) {
	return b.mgr.PublishCodexUpdate(version, installer, onProgress)
}

func (b legacyBackend) CancelCodexUpdate(context.Context) error { return b.mgr.CancelCodexUpdate() }
