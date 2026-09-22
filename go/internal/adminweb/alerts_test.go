package adminweb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestAlertsPageListsAcksAndResolves(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	device, _ := s.dbm.store.Devices().EnsureByHostname(ctx, "PC-1")
	a1, _, _ := s.dbm.store.Alerts().Open(ctx, repo.NewAlert{Kind: repo.AlertMachineOffline, Fingerprint: "m:1", Severity: "warn",
		SubjectType: "device", SubjectID: device.ID, Title: "机器 PC-1 已 30 小时未上报"})
	a2, _, _ := s.dbm.store.Alerts().Open(ctx, repo.NewAlert{Kind: repo.AlertTaskFailed, Fingerprint: "t:1", Severity: "crit",
		SubjectType: "task", SubjectID: "t1", Title: "任务 gateway_provision 已放弃（3 次尝试）"})
	old, _, _ := s.dbm.store.Alerts().Open(ctx, repo.NewAlert{Kind: repo.AlertBudget, Fingerprint: "b:1", Severity: "warn",
		SubjectType: "employee", SubjectID: "nobody", Title: "旧告警"})
	s.dbm.store.Alerts().Resolve(ctx, "b:1", "rule")
	_ = old

	page := dbGet(t, h, "/alerts", cookie)
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatalf("alerts: %d", page.Code)
	}
	open := body[strings.Index(body, "未关闭"):strings.Index(body, "最近关闭")]
	if !strings.Contains(open, "PC-1 已 30 小时未上报") || !strings.Contains(open, "machine=PC-1") || !strings.Contains(open, "已放弃") || strings.Contains(open, "旧告警") {
		t.Fatalf("open section wrong:\n%s", open)
	}
	if history := body[strings.Index(body, "最近关闭"):]; !strings.Contains(history, "旧告警") || !strings.Contains(history, "自动") {
		t.Fatal("the resolved alert is not in the history")
	}
	// Every page's nav carries the open count.
	if !strings.Contains(body, `告警 <span class="tag tag-bad">2</span>`) {
		t.Fatalf("nav badge missing: %s", firstLine(body, "告警"))
	}

	csrf := csrfFrom(t, s, cookie, "/alerts")
	if rec := dbPost(t, h, "/alerts/ack", url.Values{"csrf": {csrf}, "id": {a1.ID}}, cookie); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("ack: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	body = dbGet(t, h, "/alerts", cookie).Body.String()
	admins, _ := s.dbm.store.Admins().List(ctx)
	if !strings.Contains(body, admins[0].Username+" 已确认") {
		t.Fatal("the acknowledgement is not shown")
	}
	if rec := dbPost(t, h, "/alerts/resolve", url.Values{"csrf": {csrf}, "id": {a2.ID}}, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("resolve: %d", rec.Code)
	}
	body = dbGet(t, h, "/alerts", cookie).Body.String()
	if !strings.Contains(body, `告警 <span class="tag tag-bad">1</span>`) || !strings.Contains(body[strings.Index(body, "最近关闭"):], "已放弃") {
		t.Fatal("the resolved alert did not move to the history")
	}
	events, _, _ := s.dbm.store.Audit().Search(ctx, repo.AuditFilter{TargetType: "alert", Limit: 10})
	if len(events) != 2 {
		t.Fatalf("audit lines for ack and resolve: %d", len(events))
	}

	// A viewer reads and cannot act.
	_, viewerPassword, _ := s.dbm.auth.CreateAccount(ctx, "eve", "", "viewer")
	viewer := signInAs(t, s, "eve", viewerPassword)
	if rec := dbGet(t, h, "/alerts", viewer); rec.Code != 200 {
		t.Fatalf("viewer alerts: %d", rec.Code)
	}
	vcsrf := csrfFrom(t, s, viewer, "/alerts")
	if rec := dbPost(t, h, "/alerts/ack", url.Values{"csrf": {vcsrf}, "id": {a1.ID}}, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer ack: %d", rec.Code)
	}
}

func TestAlertSettingsKeepThePasswordWhenLeftBlank(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	csrf := csrfFrom(t, s, cookie, "/settings")

	page := dbGet(t, h, "/settings/channels", cookie).Body.String()
	if !strings.Contains(page, "通知渠道") || !strings.Contains(page, "未设置") {
		t.Fatal("the channels tab lacks its form")
	}
	if page := dbGet(t, h, "/settings/alerting", cookie).Body.String(); !strings.Contains(page, `name="offline_hours"`) {
		t.Fatal("the alerting tab lacks the thresholds form")
	}
	form := url.Values{"csrf": {csrf}, "version": {"0"},
		"webhook_enabled": {"1"}, "webhook_url": {"https://oapi.dingtalk.com/robot/send?access_token=x"}, "webhook_format": {"dingtalk"}, "webhook_secret": {"SEC123"},
		"smtp_enabled": {"1"}, "smtp_host": {"smtp.example.com"}, "smtp_port": {"587"}, "smtp_username": {"console"}, "smtp_password": {"hunter2"},
		"smtp_from": {"console@example.com"}, "smtp_to": {"ops@example.com, boss@example.com"}}
	if rec := dbPost(t, h, "/settings/alert-channels", form, cookie); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("save channels: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	cfg, version, err := repo.LoadChannelSettings(ctx, s.dbm.store.Settings())
	if err != nil || version != 1 || !cfg.Webhook.Secret.IsSet() || !cfg.SMTP.Password.IsSet() || len(cfg.SMTP.To) != 2 {
		t.Fatalf("stored: %+v v%d %v", cfg, version, err)
	}
	if string(cfg.SMTP.Password.Ciphertext) == "hunter2" || strings.Contains(string(cfg.SMTP.Password.Ciphertext), "hunter") {
		t.Fatal("the password is stored in the clear")
	}
	if got, _ := s.openSecret(ctx, "smtp_password", cfg.SMTP.Password); got != "hunter2" {
		t.Fatalf("the sealed password opens to %q", got)
	}
	// The audit line never carries the secrets.
	events, _, _ := s.dbm.store.Audit().Search(ctx, repo.AuditFilter{TargetType: "settings", Limit: 10})
	if len(events) != 1 || strings.Contains(string(events[0].After), "hunter2") || strings.Contains(string(events[0].After), "SEC123") || !strings.Contains(strings.ReplaceAll(string(events[0].After), " ", ""), `"password_set":true`) {
		t.Fatalf("audit = %+v", events)
	}

	// Saving again with blank secret fields keeps them; the page says set.
	page = dbGet(t, h, "/settings/channels", cookie).Body.String()
	if !strings.Contains(page, "已设置，留空不改") || strings.Contains(page, "hunter2") {
		t.Fatal("the page must say the password is set without showing it")
	}
	form.Set("version", "1")
	form.Set("webhook_secret", "")
	form.Set("smtp_password", "")
	form.Set("smtp_to", "ops@example.com")
	if rec := dbPost(t, h, "/settings/alert-channels", form, cookie); strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("second save: %s", rec.Header().Get("Location"))
	}
	again, version, _ := repo.LoadChannelSettings(ctx, s.dbm.store.Settings())
	if version != 2 || string(again.SMTP.Password.Ciphertext) != string(cfg.SMTP.Password.Ciphertext) || !again.Webhook.Secret.IsSet() || len(again.SMTP.To) != 1 {
		t.Fatalf("blank secrets must keep the stored ones: %+v", again)
	}
	// A stale form is refused.
	form.Set("version", "1")
	if rec := dbPost(t, h, "/settings/alert-channels", form, cookie); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("a stale version must be refused")
	}
	// Thresholds.
	if rec := dbPost(t, h, "/settings/alerts", url.Values{"csrf": {csrf}, "version": {"0"}, "enabled": {"1"}, "offline_hours": {"48"}, "budget_warn": {"90"}}, cookie); strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("save thresholds: %s", rec.Header().Get("Location"))
	}
	got, _, _ := repo.LoadAlertSettings(ctx, s.dbm.store.Settings())
	if got.OfflineAfterHours != 48 || got.BudgetWarnPercent != 90 {
		t.Fatalf("thresholds = %+v", got)
	}
	if rec := dbPost(t, h, "/settings/alerts", url.Values{"csrf": {csrf}, "version": {"1"}, "enabled": {"1"}, "offline_hours": {"0"}, "budget_warn": {"90"}}, cookie); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("an impossible threshold must be refused")
	}
	// The test button reports a refusal from the endpoint rather than
	// pretending; here the DingTalk host is not reachable from the test.
	if rec := dbPost(t, h, "/alerts/test", url.Values{"csrf": {csrf}, "channel": {"nothing"}}, cookie); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("testing an unknown channel must be an error")
	}
}

func TestRotationSettingsAndTheTokenColumn(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	csrf := csrfFrom(t, s, cookie, "/settings")
	page := dbGet(t, h, "/settings/alerting", cookie).Body.String()
	if !strings.Contains(page, "令牌轮换") || !strings.Contains(page, `name="max_age_days"`) {
		t.Fatal("the alerting tab lacks the rotation section")
	}
	if rec := dbPost(t, h, "/settings/rotation", url.Values{"csrf": {csrf}, "version": {"0"}, "enabled": {"1"}, "max_age_days": {"30"}, "per_day": {"2"}, "active_within_hours": {"48"}}, cookie); strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("save: %s", rec.Header().Get("Location"))
	}
	got, version, _ := repo.LoadRotationSettings(ctx, s.dbm.store.Settings())
	if version != 1 || !got.Enabled || got.MaxAgeDays != 30 || got.PerDay != 2 || got.ActiveWithinHours != 48 {
		t.Fatalf("stored = %+v", got)
	}
	if rec := dbPost(t, h, "/settings/rotation", url.Values{"csrf": {csrf}, "version": {"1"}, "enabled": {"1"}, "max_age_days": {"3"}, "per_day": {"2"}, "active_within_hours": {"48"}}, cookie); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("an age under a week must be refused")
	}

	// An account with a live token shows its age, and past the limit, a tag.
	dbPost(t, h, "/users/onboard", url.Values{"csrf": {csrf}, "windowsUser": {"work1"}, "budget": {"20"}, "rpm": {"60"}, "tpm": {"100000"}, "parallel": {"4"}}, cookie)
	e, _ := s.dbm.store.Employees().ByWindowsUser(ctx, "work1")
	cred, _ := s.dbm.store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: e.AuthEpoch, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	grant, _ := s.dbm.store.Grants().Create(ctx, repo.NewGrant{EmployeeID: e.ID, Epoch: e.AuthEpoch, ExternalUser: "emp-work1", KeyAlias: "emp-work1-e1", CredentialID: cred.ID})
	s.dbm.store.Grants().RecordActual(ctx, grant.ID, repo.ActualActive, "")
	list := dbGet(t, h, "/users", cookie).Body.String()
	if !strings.Contains(list, "已发放") || !strings.Contains(list, " · 0 天") || strings.Contains(list, "待轮换") {
		t.Fatalf("a fresh token: %s", firstLine(list, "已发放"))
	}
	if _, err := s.dbm.db.Pool().Exec(ctx, `update credential_versions set created_at = now() - interval '45 days'`); err != nil {
		t.Fatal(err)
	}
	list = dbGet(t, h, "/users", cookie).Body.String()
	if !strings.Contains(list, " · 45 天") || !strings.Contains(list, "待轮换") {
		t.Fatalf("an old token: %s", firstLine(list, "已发放"))
	}
}

func TestDeviceChannelPageAndMachineControls(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	csrf := csrfFrom(t, s, cookie, "/settings")

	page := dbGet(t, h, "/settings/devices", cookie).Body.String()
	if !strings.Contains(page, "设备通道") || !strings.Contains(page, `name="write_oss"`) {
		t.Fatal("the devices tab lacks its form")
	}
	if rec := dbPost(t, h, "/settings/device-channel", url.Values{"csrf": {csrf}, "version": {"0"}, "write_oss": {"1"}, "import_status": {"1"}, "enrol_cidrs": {"198.51.100.0/24\n203.0.113.9"}}, cookie); strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("save: %s", rec.Header().Get("Location"))
	}
	got, _, _ := repo.LoadDeviceChannelSettings(ctx, s.dbm.store.Settings())
	if len(got.EnrolCIDRs) != 2 || !got.WriteOSSObjects {
		t.Fatalf("stored = %+v", got)
	}
	if rec := dbPost(t, h, "/settings/device-channel", url.Values{"csrf": {csrf}, "version": {"1"}, "write_oss": {"1"}, "import_status": {"1"}, "enrol_cidrs": {"not-a-network"}}, cookie); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("a bad network must be refused")
	}
	// The API now refuses enrolment from outside the list.
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/enrol", strings.NewReader(`{"hostname":"PC-1"}`))
	req.RemoteAddr = "192.0.2.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("enrol from outside the list: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/agent/v1/enrol", strings.NewReader(`{"hostname":"PC-1","agentVersion":"1.3.0"}`))
	req.RemoteAddr = "203.0.113.9:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("enrol from inside the list: %d %s", rec.Code, rec.Body.String())
	}
	var enrolled struct {
		Token string `json:"deviceToken"`
	}
	json.Unmarshal(rec.Body.Bytes(), &enrolled)

	// The overview shows the channel; the machine page shows the token and
	// offers the two controls.
	if body := dbGet(t, h, "/overview", cookie).Body.String(); !strings.Contains(body, ">API<") {
		t.Fatal("the overview lacks the channel column")
	}
	machine := dbGet(t, h, "/machines/detail?machine=PC-1", cookie).Body.String()
	if !strings.Contains(machine, "直连控制台") || !strings.Contains(machine, "签发于") || !strings.Contains(machine, "203.0.113.9") || !strings.Contains(machine, "允许重新注册") {
		t.Fatal("the machine page lacks the channel section")
	}
	// The log tail the agent sent is what the log page shows.
	req = httptest.NewRequest(http.MethodPost, "/agent/v1/log", strings.NewReader("agent log line\n"))
	req.Header.Set("Authorization", "Bearer "+enrolled.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("log: %d", rec.Code)
	}
	if body := dbGet(t, h, "/log?machine=PC-1", cookie).Body.String(); !strings.Contains(body, "agent log line") {
		t.Fatal("the log page must show the tail the agent sent")
	}

	// Allow re-enrolment, then revoke: both audited, both change the API's answer.
	if rec := dbPost(t, h, "/machines/allow-reenrol", url.Values{"csrf": {csrf}, "machine": {"PC-1"}}, cookie); strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("allow: %s", rec.Header().Get("Location"))
	}
	d, _ := s.dbm.store.Devices().ByHostname(ctx, "PC-1")
	if d.ReenrolAllowedUntil == nil {
		t.Fatal("the window did not open")
	}
	if rec := dbPost(t, h, "/machines/revoke-token", url.Values{"csrf": {csrf}, "machine": {"PC-1"}}, cookie); strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("revoke: %s", rec.Header().Get("Location"))
	}
	req = httptest.NewRequest(http.MethodGet, "/agent/v1/config", nil)
	req.Header.Set("Authorization", "Bearer "+enrolled.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after revocation: %d", rec.Code)
	}
	events, _, _ := s.dbm.store.Audit().Search(ctx, repo.AuditFilter{TargetType: "device", Limit: 10})
	actions := ""
	for _, e := range events {
		actions += e.Action + " "
	}
	if !strings.Contains(actions, "machine.allow_reenrol") || !strings.Contains(actions, "machine.revoke_token") || !strings.Contains(actions, "device.enrol") {
		t.Fatalf("audit actions = %s", actions)
	}
}
