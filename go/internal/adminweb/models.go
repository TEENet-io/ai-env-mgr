package adminweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// modelDelivery is the overview's 模型下发 block: what changed on the gateway
// since the fleet was last given its model configuration, the controls to
// deliver it (to one employee first, then everybody), and where the last
// delivery has got to, machine by machine.
type modelDelivery struct {
	Version        int
	NeverDelivered bool
	FleetAt        string
	FleetBy        string
	Added, Removed []string
	// MetaChanged: the same models, but something the picker shows about
	// them (display name, context window, reasoning levels) is different.
	MetaChanged bool
	Changed     bool

	Last      *ops.LastDelivery
	LastAt    string
	Rows      []deliveryRow
	Received  int
	Employees []repo.Employee // for the test delivery's choice
	Devices   []deliveryDeviceOption
	History   []deliveryHistory
}

type deliveryHistory struct {
	At, By, Scope string
	Rows          []deliveryHistoryRow
}

type deliveryHistoryRow struct {
	WindowsUser, Hostname, State, Detail string
}

type deliveryDeviceOption struct {
	ID, Hostname, WindowsUser string
}

// deliveryRow is one machine of one employee in the last delivery.
type deliveryRow struct {
	EmployeeID  string
	DeviceID    string
	WindowsUser string
	Hostname    string
	State       string
	Sev         string // ok / warn / bad / muted
	Detail      string
	Retries     int
}

func (s *Server) loadModelDelivery(ctx context.Context, data *pageData) {
	if s.dbm == nil || !data.GatewayEnabled || data.GatewayUnusable != "" {
		return
	}
	state, version, err := s.dbm.ops.CatalogDeliveryState(ctx)
	if err != nil {
		data.Error = "could not read the model delivery: " + err.Error()
		return
	}
	deliverable, err := s.deliverableModels(ctx, data.GatewayModels)
	if err != nil {
		data.Error = "could not read the channels: " + err.Error()
		return
	}
	now := ops.SnapshotOf(deliverable)
	v := &modelDelivery{Version: version, NeverDelivered: state.FleetAt == ""}
	if state.FleetAt != "" {
		v.FleetAt, v.FleetBy = localTime(state.FleetAt), state.FleetBy
		v.Added, v.Removed = diffNames(state.Fleet.Models, now.Models)
		v.MetaChanged = len(v.Added) == 0 && len(v.Removed) == 0 && state.Fleet.Digest != now.Digest
	}
	v.Changed = v.NeverDelivered || len(v.Added) > 0 || len(v.Removed) > 0 || v.MetaChanged
	if all, err := s.dbm.store.Employees().List(ctx, repo.EmployeeFilter{}); err == nil {
		for _, e := range all {
			if e.Active() {
				v.Employees = append(v.Employees, e)
			}
		}
	}
	if bindings, err := s.dbm.store.Bindings().ListOpen(ctx); err == nil {
		for _, b := range bindings {
			e, err := s.dbm.store.Employees().ByID(ctx, b.EmployeeID)
			if err != nil || !e.Active() {
				continue
			}
			d, err := s.dbm.store.Devices().ByID(ctx, b.DeviceID)
			if err == nil && d.Status != repo.DeviceRevoked {
				v.Devices = append(v.Devices, deliveryDeviceOption{ID: d.ID, Hostname: d.Hostname, WindowsUser: e.WindowsUser})
			}
		}
		sort.Slice(v.Devices, func(i, j int) bool { return v.Devices[i].Hostname < v.Devices[j].Hostname })
	}
	if state.Last.At != "" {
		last := state.Last
		v.Last, v.LastAt = &last, localTime(last.At)
		v.Rows = s.deliveryRows(ctx, last)
		for _, r := range v.Rows {
			if r.Sev == "ok" {
				v.Received++
			}
		}
	}
	for i := len(state.History) - 1; i >= 0; i-- {
		old := state.History[i]
		item := deliveryHistory{At: localTime(old.At), By: old.By, Scope: old.Scope}
		for _, target := range old.Targets {
			row := deliveryHistoryRow{WindowsUser: target.WindowsUser, Hostname: target.Hostname}
			task, err := s.dbm.store.Tasks().ByID(ctx, old.Tasks[target.EmployeeID])
			if err != nil {
				row.State = "任務不可讀"
			} else {
				switch task.Status {
				case repo.TaskSucceeded:
					row.State = "配置已生成"
				case repo.TaskFailed, repo.TaskSuperseded:
					row.State, row.Detail = "生成失敗", task.LastError
				default:
					row.State = "生成中"
				}
			}
			item.Rows = append(item.Rows, row)
		}
		v.History = append(v.History, item)
	}
	data.ModelDelivery = v
}

// deliveryRows says, for every machine of every employee in the delivery,
// whether it has the new configuration. A machine has it once it has
// synced after the export finished without a credentials error: every sync
// fetches the bundle if it changed, on either channel.
func (s *Server) deliveryRows(ctx context.Context, last ops.LastDelivery) []deliveryRow {
	var rows []deliveryRow
	targets := last.Targets
	if len(targets) == 0 { // Older deliveries did not store immutable targets.
		for employeeID := range last.Tasks {
			e, err := s.dbm.store.Employees().ByID(ctx, employeeID)
			if err != nil {
				continue
			}
			bindings, err := s.dbm.store.Bindings().OpenByEmployee(ctx, employeeID)
			if err != nil || len(bindings) == 0 {
				targets = append(targets, ops.CatalogDeliveryTarget{EmployeeID: employeeID, WindowsUser: e.WindowsUser})
				continue
			}
			for _, b := range bindings {
				d, err := s.dbm.store.Devices().ByID(ctx, b.DeviceID)
				if err == nil {
					targets = append(targets, ops.CatalogDeliveryTarget{EmployeeID: employeeID, WindowsUser: e.WindowsUser, DeviceID: d.ID, Hostname: d.Hostname})
				}
			}
		}
	}
	for _, target := range targets {
		employeeID, taskID := target.EmployeeID, last.Tasks[target.EmployeeID]
		row := deliveryRow{EmployeeID: employeeID, DeviceID: target.DeviceID, WindowsUser: target.WindowsUser, Hostname: target.Hostname, Retries: last.Retries[employeeID]}
		task, err := s.dbm.store.Tasks().ByID(ctx, taskID)
		if err != nil {
			row.State, row.Sev = "任務不見了", "bad"
			rows = append(rows, row)
			continue
		}
		switch task.Status {
		case repo.TaskSucceeded:
		case repo.TaskFailed, repo.TaskSuperseded:
			row.State, row.Sev, row.Detail = "生成失敗", "bad", task.LastError
			rows = append(rows, row)
			continue
		default:
			row.State, row.Sev = "生成中", "muted"
			rows = append(rows, row)
			continue
		}
		if target.DeviceID == "" {
			row.State, row.Sev, row.Detail = "未綁定機器", "muted", "配置已生成，綁定機器後送達"
			rows = append(rows, row)
			continue
		}
		binding, err := s.dbm.store.Bindings().Open(ctx, target.DeviceID)
		if err != nil || binding.EmployeeID != employeeID {
			row.State, row.Sev, row.Detail = "目標已變更", "bad", "機器已解綁或轉給其他員工；請重新下發"
			rows = append(rows, row)
			continue
		}
		row.State, row.Sev, row.Detail = arrival(s.dbm.store, ctx, target.DeviceID, employeeID, task.FinishedAt)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].WindowsUser != rows[j].WindowsUser {
			return rows[i].WindowsUser < rows[j].WindowsUser
		}
		return rows[i].Hostname < rows[j].Hostname
	})
	return rows
}

func arrival(store repo.Store, ctx context.Context, deviceID, employeeID string, exported *time.Time) (state, sev, detail string) {
	report, err := store.Reports().Get(ctx, deviceID)
	if errors.Is(err, repo.ErrNotFound) || (err == nil && report.LastSyncAt == nil) {
		return "等待同步", "warn", "機器從未上報"
	}
	if err != nil {
		return "讀不到機器狀態", "bad", err.Error()
	}
	online := time.Since(*report.LastSyncAt) <= onlineWithin
	if exported == nil || !report.LastSyncAt.After(*exported) {
		if online {
			return "等待同步", "warn", "在線，下一次同步會取到"
		}
		return "等待同步", "warn", "離線，開機後取到；最後同步 " + report.LastSyncAt.Local().Format("01-02 15:04")
	}
	var status model.Status
	if err := json.Unmarshal(report.Report, &status); err != nil {
		return "狀態無效", "bad", "Agent 狀態報告無法解析"
	}
	if e, err := store.Employees().ByID(ctx, employeeID); err == nil && !strings.EqualFold(status.BoundUser, e.WindowsUser) {
		return "同步到錯誤用戶", "bad", "Agent 上報的綁定用戶與下發目標不同"
	}
	for _, e := range status.Errors {
		if strings.HasPrefix(e, "credentials") {
			return "同步出錯", "bad", e
		}
	}
	if device, err := store.Devices().ByID(ctx, deviceID); err == nil && device.Channel == repo.ChannelAPI {
		etag, err := store.CredentialBundles().LiveETag(ctx, employeeID)
		if err != nil {
			return "讀不到目標配置", "bad", err.Error()
		}
		if !status.CredsApplied || status.CredsETag != etag {
			return "未收到新配置", "bad", "Agent 已同步，但尚未確認新模型清單；可重試下發"
		}
	}
	if !status.CredsApplied {
		return "未確認落盤", "bad", "Agent 未確認模型配置已寫入用戶目錄"
	}
	return "已收到", "ok", "同步於 " + report.LastSyncAt.Local().Format("01-02 15:04") + "；員工重啟 Codex 後生效"
}

// diffNames is what was added to and removed from a sorted list of names.
func diffNames(before, after []string) (added, removed []string) {
	had := map[string]bool{}
	for _, n := range before {
		had[n] = true
	}
	has := map[string]bool{}
	for _, n := range after {
		has[n] = true
		if !had[n] {
			added = append(added, n)
		}
	}
	for _, n := range before {
		if !has[n] {
			removed = append(removed, n)
		}
	}
	return added, removed
}

func localTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Local().Format("01-02 15:04")
}

// actionModelsDeliver is 下发模型配置: to one employee (a test delivery) or,
// with no employee chosen, to everybody. The catalog is read from the
// gateway now, not taken from the form.
func (s *Server) actionModelsDeliver(sess *session, r *http.Request) (string, error) {
	gw, err := s.gateway()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(r.Context(), gatewayModelsTimeout)
	defer cancel()
	models, err := gw.Models(ctx)
	if err != nil {
		return "", fmt.Errorf("讀不到網關模型清單，沒有下發：%w", err)
	}
	if models, err = s.deliverableModels(r.Context(), models); err != nil {
		return "", err
	}
	if len(models) == 0 {
		return "", errors.New("網關沒有可下發的模型（沒有可展示的模型，或渠道全部暫停），沒有下發")
	}
	var n int
	switch formValue(r, "scope") {
	case "all":
		n, err = s.dbm.ops.DeliverCatalog(r.Context(), ops.SnapshotOf(models), "", formInt(r, "version"), sess.actor, s.clientKey(r))
	case "test":
		employeeID := formValue(r, "employee")
		if employeeID == "" {
			return "", errors.New("先選一個測試員工")
		}
		n, err = s.dbm.ops.DeliverCatalog(r.Context(), ops.SnapshotOf(models), employeeID, formInt(r, "version"), sess.actor, s.clientKey(r))
	case "batch":
		n, err = s.dbm.ops.DeliverCatalogBatch(r.Context(), ops.SnapshotOf(models), r.PostForm["employees"], r.PostForm["devices"], formInt(r, "version"), sess.actor, s.clientKey(r))
	default:
		return "", errors.New("選擇下發範圍")
	}
	if err != nil {
		return "", err
	}
	logAudit(s.clientKey(r), "delivered the model configuration to %d employee(s)", n)
	return fmt.Sprintf("已給 %d 位員工重新生成模型配置；下面的表顯示每台機器是否已收到，收到後通知員工重啟 Codex", n), nil
}

func (s *Server) actionModelsRetry(sess *session, r *http.Request) (string, error) {
	gw, err := s.gateway()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(r.Context(), gatewayModelsTimeout)
	defer cancel()
	models, err := gw.Models(ctx)
	if err != nil {
		return "", fmt.Errorf("讀不到網關模型清單，沒有重試：%w", err)
	}
	models, err = s.deliverableModels(r.Context(), models)
	if err != nil {
		return "", err
	}
	if err := s.dbm.ops.RetryCatalogDelivery(r.Context(), ops.SnapshotOf(models), formValue(r, "employee"), formInt(r, "version"), sess.actor, s.clientKey(r)); err != nil {
		return "", err
	}
	return "已重新生成所選員工的模型配置；請在下方查看各設備的同步結果", nil
}
