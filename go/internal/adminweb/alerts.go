package adminweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/notify"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/worker"
)

// alertsPage is the open alerts and recent history.
type alertsPage struct {
	Open                 []alertRow
	History              []alertRow
	LastEval, LastNotify string
}

type alertRow struct {
	repo.Alert
	Sev     string // tag class: warn | bad
	Subject string // display name
	Link    string
	Age     string
	Notify  string // 已通知 / 未通知 / 发送失败：…
}

// alertSettingsView is the thresholds form.
type alertSettingsView struct {
	repo.AlertSettings
	Version int
}

type dataRetentionView struct {
	repo.DataRetentionSettings
	Version int
}

// channelSettingsView is the device channel tab.
type channelSettingsView struct {
	repo.DeviceChannelSettings
	Version   int
	CIDRsText string
}

// rotationView is the token rotation form.
type rotationView struct {
	repo.RotationSettings
	Version int
}

// channelView is the channels form: everything but the secrets, which are
// only ever "set" or "not set".
type channelView struct {
	repo.ChannelSettings
	Version     int
	SecretSet   bool
	PasswordSet bool
	ToText      string
}

// openAlertCount is drawn in the nav on every page.
func (s *Server) openAlertCount() int {
	if s.dbm == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n, err := s.dbm.store.Alerts().CountOpen(ctx)
	if err != nil {
		return 0
	}
	return n
}

func (s *Server) alertRows(r *http.Request, alerts []repo.Alert, now time.Time) []alertRow {
	rows := make([]alertRow, 0, len(alerts))
	ctx := r.Context()
	for _, a := range alerts {
		row := alertRow{Alert: a, Sev: "warn", Subject: a.SubjectID, Age: humanDuration(now.Sub(a.OpenedAt))}
		if a.Severity == repo.SeverityCrit {
			row.Sev = "bad"
		}
		switch a.SubjectType {
		case "device":
			if d, err := s.dbm.store.Devices().ByID(ctx, a.SubjectID); err == nil {
				row.Subject, row.Link = d.Hostname, "/machines/detail?machine="+url.QueryEscape(d.Hostname)
			}
		case "employee":
			if e, err := s.dbm.store.Employees().ByID(ctx, a.SubjectID); err == nil {
				row.Subject, row.Link = e.WindowsUser, "/users/detail?user="+url.QueryEscape(e.WindowsUser)
			}
		case "target":
			if t, err := s.dbm.store.Releases().TargetByID(ctx, a.SubjectID); err == nil && t.RolloutID != "" {
				row.Subject, row.Link = "發布任務", "/rollouts/detail?id="+url.QueryEscape(t.RolloutID)
			}
		case "task":
			row.Subject, row.Link = "任務", "/tasks"
		case "grant":
			if g, err := s.dbm.store.Grants().ByKeyAlias(ctx, "", a.SubjectID); err == nil {
				if e, err := s.dbm.store.Employees().ByID(ctx, g.EmployeeID); err == nil {
					row.Subject, row.Link = e.WindowsUser, "/users/detail?user="+url.QueryEscape(e.WindowsUser)
				}
			}
		case "settings":
			if a.SubjectID == "model_catalog" {
				row.Subject, row.Link = "模型目錄", "/models"
			}
		}
		switch {
		case a.NotifiedAt != nil:
			row.Notify = "已通知 " + a.NotifiedAt.Local().Format("01-02 15:04")
		case a.NotifyError != "":
			row.Notify = fmt.Sprintf("發送失敗 %d 次：%s", a.NotifyTries, a.NotifyError)
		default:
			row.Notify = "待通知"
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "alerts")
	data.Tab = "alerts"
	now := time.Now()
	page := &alertsPage{LastEval: s.lastRunOf(r, worker.TaskAlertEval), LastNotify: s.lastRunOf(r, worker.TaskAlertNotify)}
	open, err := s.dbm.store.Alerts().ListOpen(r.Context())
	if err != nil {
		data.Error = "could not read the alerts"
	}
	page.Open = s.alertRows(r, open, now)
	recent, err := s.dbm.store.Alerts().ListRecent(r.Context(), 200)
	if err != nil {
		data.Error = "could not read the alerts"
	}
	var history []repo.Alert
	for _, a := range recent {
		if !a.Open() {
			history = append(history, a)
		}
	}
	page.History = s.alertRows(r, history, now)
	data.AlertsPage = page
	s.render(w, "alerts.html", http.StatusOK, data)
}

func (s *Server) actionAlertAck(sess *session, r *http.Request) error {
	_, err := s.dbm.ops.AckAlert(r.Context(), formValue(r, "id"), sess.actor, s.clientKey(r))
	return err
}

func (s *Server) actionAlertResolve(sess *session, r *http.Request) error {
	_, err := s.dbm.ops.ResolveAlert(r.Context(), formValue(r, "id"), sess.actor, s.clientKey(r))
	return err
}

// actionAlertTest sends a fixed message through one channel, enabled or
// not, and says what happened. Nothing is stored.
func (s *Server) actionAlertTest(sess *session, r *http.Request) (string, error) {
	cfg, _, err := repo.LoadChannelSettings(r.Context(), s.dbm.store.Settings())
	if err != nil {
		return "", err
	}
	channels, err := s.channelsFrom(r.Context(), cfg, false)
	if err != nil {
		return "", err
	}
	want := formValue(r, "channel")
	for _, ch := range channels {
		if ch.Name() != want {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		m := notify.Message{Title: "測試消息", Severity: repo.SeverityWarn, Subject: sess.actor,
			Detail: "這是 windows-control 控制台發出的測試告警；收到即渠道可用。", URL: s.consoleURL() + "/alerts", At: time.Now()}
		if err := ch.Send(ctx, m); err != nil {
			return "", fmt.Errorf("%s 發送失敗：%w", want, err)
		}
		return want + " 已發送測試消息", nil
	}
	return "", errors.New("先填好並保存這個渠道，再發測試")
}

func (s *Server) actionAlertSettings(sess *session, r *http.Request) error {
	before, version, err := repo.LoadAlertSettings(r.Context(), s.dbm.store.Settings())
	if err != nil {
		return err
	}
	after := repo.AlertSettings{
		Enabled:           formValue(r, "enabled") == "1",
		OfflineAfterHours: formInt(r, "offline_hours"),
		BudgetWarnPercent: formInt(r, "budget_warn"),
		CleanupAfterDays:  formInt(r, "cleanup_days"),
	}
	if after.CleanupAfterDays == 0 {
		// A form from before the field existed keeps what is stored.
		after.CleanupAfterDays = before.CleanupAfterDays
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if v := formInt(r, "version"); v != version {
		return errors.New("這頁的設定在你打開後已被別人改過，請刷新後重試")
	}
	value, _ := json.Marshal(after)
	if err := s.dbm.ops.SaveSetting(r.Context(), repo.SettingAlerts, value, version, ops.ActionAlertSettings, before, after, sess.actor, s.clientKey(r)); err != nil {
		return err
	}
	return worker.EnqueueAlertEval(r.Context(), s.dbm.store, time.Now())
}

// actionAlertChannels saves the channels. A blank secret field keeps the
// stored secret; the audit line carries the settings without either.
func (s *Server) actionAlertChannels(sess *session, r *http.Request) error {
	ctx := r.Context()
	before, version, err := repo.LoadChannelSettings(ctx, s.dbm.store.Settings())
	if err != nil {
		return err
	}
	if v := formInt(r, "version"); v != version {
		return errors.New("這頁的設定在你打開後已被別人改過，請刷新後重試")
	}
	after := before
	after.Webhook.Enabled = formValue(r, "webhook_enabled") == "1"
	after.Webhook.URL = formValue(r, "webhook_url")
	after.Webhook.Format = formValue(r, "webhook_format")
	if after.Webhook.URL != "" {
		u, err := url.Parse(after.Webhook.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("webhook 地址必須是 http(s) URL")
		}
	}
	switch after.Webhook.Format {
	case notify.FormatGeneric, notify.FormatDingTalk, notify.FormatFeishu:
	default:
		return errors.New("未知的 webhook 格式")
	}
	if secret := formValue(r, "webhook_secret"); secret != "" {
		if after.Webhook.Secret, err = s.sealSecret(ctx, "webhook_secret", secret); err != nil {
			return err
		}
	} else if formValue(r, "webhook_secret_clear") == "1" {
		after.Webhook.Secret = repo.Sealed{}
	}
	after.SMTP.Enabled = formValue(r, "smtp_enabled") == "1"
	after.SMTP.Host = formValue(r, "smtp_host")
	after.SMTP.Port = formInt(r, "smtp_port")
	after.SMTP.Username = formValue(r, "smtp_username")
	after.SMTP.From = formValue(r, "smtp_from")
	after.SMTP.To = splitRecipients(formValue(r, "smtp_to"))
	if after.SMTP.Port < 0 || after.SMTP.Port > 65535 {
		return errors.New("SMTP 端口不合法")
	}
	if password := formValue(r, "smtp_password"); password != "" {
		if after.SMTP.Password, err = s.sealSecret(ctx, "smtp_password", password); err != nil {
			return err
		}
	} else if formValue(r, "smtp_password_clear") == "1" {
		after.SMTP.Password = repo.Sealed{}
	}
	if after.Webhook.Enabled && after.Webhook.URL == "" {
		return errors.New("要啟用 webhook，先填地址")
	}
	if after.SMTP.Enabled && (after.SMTP.Host == "" || after.SMTP.From == "" || len(after.SMTP.To) == 0) {
		return errors.New("要啟用郵件，先填伺服器、發件人和收件人")
	}
	value, err := json.Marshal(after)
	if err != nil {
		return err
	}
	return s.dbm.ops.SaveSetting(ctx, repo.SettingAlertChannels, value, version, ops.ActionAlertChannels,
		redactChannels(before), redactChannels(after), sess.actor, s.clientKey(r))
}

// redactChannels is the settings as they may appear in the audit trail:
// the secrets replaced by whether they are set.
func redactChannels(c repo.ChannelSettings) map[string]any {
	return map[string]any{
		"webhook": map[string]any{"enabled": c.Webhook.Enabled, "url": c.Webhook.URL, "format": c.Webhook.Format, "secret_set": c.Webhook.Secret.IsSet()},
		"smtp": map[string]any{"enabled": c.SMTP.Enabled, "host": c.SMTP.Host, "port": c.SMTP.Port, "username": c.SMTP.Username,
			"from": c.SMTP.From, "to": c.SMTP.To, "password_set": c.SMTP.Password.IsSet()},
	}
}

func splitRecipients(text string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == ' ' || r == '\r' || r == '\t' }) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// settingsExtras loads the alert sections of the settings page.
func (s *Server) settingsExtras(r *http.Request, data *pageData) {
	if s.dbm == nil {
		return
	}
	ctx := r.Context()
	if a, version, err := repo.LoadAlertSettings(ctx, s.dbm.store.Settings()); err == nil {
		data.AlertSettings = &alertSettingsView{AlertSettings: a, Version: version}
	} else {
		data.Error = "讀取告警設定失敗：" + err.Error()
	}
	if rs, version, err := repo.LoadRotationSettings(ctx, s.dbm.store.Settings()); err == nil {
		data.Rotation = &rotationView{RotationSettings: rs, Version: version}
	} else {
		data.Error = "讀取輪換設定失敗：" + err.Error()
	}
	if dc, version, err := repo.LoadDeviceChannelSettings(ctx, s.dbm.store.Settings()); err == nil {
		data.DeviceChannel = &channelSettingsView{DeviceChannelSettings: dc, Version: version, CIDRsText: strings.Join(dc.EnrolCIDRs, "\n")}
	} else {
		data.Error = "讀取設備通道設定失敗：" + err.Error()
	}
	if gt, version, err := repo.LoadGatewayTeamSettings(ctx, s.dbm.store.Settings()); err == nil {
		data.GatewayTeam = &gatewayTeamView{GatewayTeamSettings: gt, Version: version}
		if s.opts.GatewayAdminURL != "" {
			data.GatewayTeam.TeamID, data.GatewayTeam.FromToken = s.teamID(), true
		}
	} else {
		data.Error = "讀取組織設定失敗：" + err.Error()
	}
	if tools, version, err := repo.LoadTicketToolSettings(ctx, s.dbm.store.Settings()); err == nil {
		data.TicketTools = &ticketToolsView{Text: strings.Join(tools.Names, "\n"), Version: version}
	} else {
		data.Error = "讀取工單工具清單失敗：" + err.Error()
	}
	if c, version, err := repo.LoadChannelSettings(ctx, s.dbm.store.Settings()); err == nil {
		if c.Webhook.Format == "" {
			c.Webhook.Format = notify.FormatGeneric
		}
		if c.SMTP.Port == 0 {
			c.SMTP.Port = 587
		}
		data.Channels = &channelView{ChannelSettings: c, Version: version,
			SecretSet: c.Webhook.Secret.IsSet(), PasswordSet: c.SMTP.Password.IsSet(), ToText: strings.Join(c.SMTP.To, ", ")}
	} else {
		data.Error = "讀取告警渠道失敗：" + err.Error()
	}
}

func (s *Server) actionRotationSettings(sess *session, r *http.Request) error {
	before, version, err := repo.LoadRotationSettings(r.Context(), s.dbm.store.Settings())
	if err != nil {
		return err
	}
	after := repo.RotationSettings{
		Enabled:           formValue(r, "enabled") == "1",
		MaxAgeDays:        formInt(r, "max_age_days"),
		PerDay:            formInt(r, "per_day"),
		ActiveWithinHours: formInt(r, "active_within_hours"),
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if v := formInt(r, "version"); v != version {
		return errors.New("這頁的設定在你打開後已被別人改過，請刷新後重試")
	}
	value, _ := json.Marshal(after)
	return s.dbm.ops.SaveSetting(r.Context(), repo.SettingRotation, value, version, ops.ActionRotationSettings, before, after, sess.actor, s.clientKey(r))
}

func (s *Server) actionDeviceChannel(sess *session, r *http.Request) error {
	before, version, err := repo.LoadDeviceChannelSettings(r.Context(), s.dbm.store.Settings())
	if err != nil {
		return err
	}
	if v := formInt(r, "version"); v != version {
		return errors.New("這頁的設定在你打開後已被別人改過，請刷新後重試")
	}
	after := repo.DeviceChannelSettings{
		EnrolCIDRs: splitRecipients(formValue(r, "enrol_cidrs")),
	}
	nets, err := after.ParsedCIDRs()
	if err != nil {
		return err
	}
	value, _ := json.Marshal(after)
	if err := s.dbm.ops.SaveSetting(r.Context(), repo.SettingDeviceChannel, value, version, ops.ActionDeviceChannel, before, after, sess.actor, s.clientKey(r)); err != nil {
		return err
	}
	s.dbm.devices.SetEnrolCIDRs(nets)
	return nil
}

type ticketToolsView struct {
	Text    string
	Version int
}

type gatewayTeamView struct {
	repo.GatewayTeamSettings
	Version int
	// FromToken: the team is the one the tenant-admin token manages, shown
	// rather than edited here.
	FromToken bool
}

// actionGatewayTeam sets the gateway team and moves every employee into it.
// Saving the team only changes how new tokens are named; the employees
// already issued keep theirs until re-issued, so the move runs right after
// (ops.MoveToTeam). Saving the same team again re-runs the move, which is how
// one that stopped half way is finished. "preview" lists who would be
// re-issued and changes nothing.
func (s *Server) actionGatewayTeam(sess *session, r *http.Request) (string, error) {
	if s.opts.GatewayAdminURL != "" {
		return "", errors.New("組織由網關憑證決定，不能在這裡修改")
	}
	ctx := r.Context()
	before, version, err := repo.LoadGatewayTeamSettings(ctx, s.dbm.store.Settings())
	if err != nil {
		return "", err
	}
	if v := formInt(r, "version"); v != version {
		return "", errors.New("這頁的設定在你打開後已被別人改過，請刷新後重試")
	}
	team := strings.TrimSpace(formValue(r, "team_id"))
	if team == "" {
		// Back to no team would need every employee moved out again.
		return "", errors.New("請填寫團隊 ID；設定後不能清空")
	}
	if err := litellm.ValidateTeamID(team); err != nil {
		return "", errors.New("團隊 ID 只能用小寫字母、數字和下劃線，2–32 位")
	}
	if formValue(r, "preview") == "1" {
		moved, err := s.dbm.ops.MoveToTeam(ctx, team, true, sess.actor, s.clientKey(r))
		if err != nil {
			return "", err
		}
		if len(moved) == 0 {
			return "預覽：沒有需要重新簽發的員工", nil
		}
		return fmt.Sprintf("預覽：保存後將為 %d 人重新簽發令牌（%s），舊令牌隨即失效", len(moved), strings.Join(moved, "、")), nil
	}
	if team != before.TeamID {
		after := repo.GatewayTeamSettings{TeamID: team}
		value, _ := json.Marshal(after)
		if err := s.dbm.ops.SaveSetting(ctx, repo.SettingGatewayTeam, value, version, ops.ActionGatewayTeam, before, after, sess.actor, s.clientKey(r)); err != nil {
			return "", err
		}
		// Before the move: its re-issues must be minted in the new team.
		s.dbm.team.Store(&team)
	}
	moved, err := s.dbm.ops.MoveToTeam(ctx, team, false, sess.actor, s.clientKey(r))
	if err != nil {
		return "", fmt.Errorf("團隊已保存，但遷移中斷（已遷移 %d 人）：%w；再點一次保存可以繼續", len(moved), err)
	}
	if len(moved) == 0 {
		return "組織已設為 " + team + "，沒有需要遷移的員工", nil
	}
	return fmt.Sprintf("組織已設為 %s，已為 %d 人重新簽發令牌（%s），機器下次同步時換上新令牌", team, len(moved), strings.Join(moved, "、")), nil
}

func (s *Server) actionAllowReenrol(sess *session, r *http.Request) (string, error) {
	until, err := s.dbm.ops.AllowReenrol(r.Context(), formValue(r, "machine"), sess.actor, s.clientKey(r))
	if err != nil {
		return "", err
	}
	return "這台機器在 " + until.Local().Format("01-02 15:04") + " 前可以註冊", nil
}

func (s *Server) actionRevokeToken(sess *session, r *http.Request) (string, error) {
	if err := s.dbm.ops.RevokeDeviceToken(r.Context(), formValue(r, "machine"), sess.actor, s.clientKey(r)); err != nil {
		return "", err
	}
	return "令牌已吊銷；機器下一次請求會被拒，隨後一小時內可以自己重新註冊", nil
}
