package dbstore

import (
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestAlertsOpenOncePerFingerprintAndResolve(t *testing.T) {
	s, ctx := newTestStore(t)
	n := repo.NewAlert{Kind: repo.AlertMachineOffline, Fingerprint: "machine_offline:d1", Severity: repo.SeverityWarn,
		SubjectType: "device", SubjectID: "d1", Title: "PC-1 已 26 小时未上报"}
	first, created, err := s.Alerts().Open(ctx, n)
	if err != nil || !created {
		t.Fatalf("first open: created=%v err=%v", created, err)
	}
	again, created, err := s.Alerts().Open(ctx, n)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second open must return the open one: created=%v id=%s err=%v", created, again.ID, err)
	}
	if n, _ := s.Alerts().CountOpen(ctx); n != 1 {
		t.Fatalf("open count = %d", n)
	}
	waiting, _ := s.Alerts().Unnotified(ctx, 10, 50)
	if len(waiting) != 1 {
		t.Fatalf("unnotified = %d", len(waiting))
	}
	if err := s.Alerts().MarkNotified(ctx, first.ID, time.Time{}, "smtp: connection refused"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Alerts().ByID(ctx, first.ID)
	if a.NotifyTries != 1 || a.NotifyError == "" || a.NotifiedAt != nil {
		t.Fatalf("after a failed send: %+v", a)
	}
	if err := s.Alerts().MarkNotified(ctx, first.ID, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if waiting, _ = s.Alerts().Unnotified(ctx, 10, 50); len(waiting) != 0 {
		t.Fatal("a notified alert is still listed as waiting")
	}
	acked, err := s.Alerts().Ack(ctx, first.ID, "zhang")
	if err != nil || acked.AckedBy != "zhang" || acked.AckedAt == nil {
		t.Fatalf("ack: %+v %v", acked, err)
	}
	if n, _ := s.Alerts().Resolve(ctx, "machine_offline:d1", "rule"); n != 1 {
		t.Fatalf("resolve closed %d", n)
	}
	if n, _ := s.Alerts().Resolve(ctx, "machine_offline:d1", "rule"); n != 0 {
		t.Fatal("resolving again must be a no-op")
	}
	// Once resolved, the same condition opens a fresh alert.
	third, created, _ := s.Alerts().Open(ctx, n)
	if !created || third.ID == first.ID {
		t.Fatal("a resolved fingerprint must open anew")
	}
	if open, _ := s.Alerts().ListOpen(ctx); len(open) != 1 || open[0].ID != third.ID {
		t.Fatalf("open = %+v", open)
	}
	if recent, _ := s.Alerts().ListRecent(ctx, 10); len(recent) != 2 {
		t.Fatalf("recent = %d", len(recent))
	}
	byID, err := s.Alerts().ResolveByID(ctx, third.ID, "li")
	if err != nil || byID.ResolvedBy != "li" {
		t.Fatalf("resolve by id: %+v %v", byID, err)
	}
}

func TestAlertSettingsDefaultUntilStored(t *testing.T) {
	s, ctx := newTestStore(t)
	got, version, err := repo.LoadAlertSettings(ctx, s.Settings())
	if err != nil || version != 0 || got != repo.DefaultAlertSettings() {
		t.Fatalf("defaults: %+v v%d %v", got, version, err)
	}
	if _, err := s.Settings().Set(ctx, repo.SettingAlerts, []byte(`{"enabled":false,"offline_after_hours":48,"budget_warn_percent":90}`), 0, "zhang"); err != nil {
		t.Fatal(err)
	}
	got, version, err = repo.LoadAlertSettings(ctx, s.Settings())
	if err != nil || version != 1 || got.Enabled || got.OfflineAfterHours != 48 || got.BudgetWarnPercent != 90 {
		t.Fatalf("stored: %+v v%d %v", got, version, err)
	}
	if (repo.AlertSettings{OfflineAfterHours: 0, BudgetWarnPercent: 80}).Validate() == nil ||
		(repo.AlertSettings{OfflineAfterHours: 24, BudgetWarnPercent: 100}).Validate() == nil {
		t.Fatal("bad thresholds must be refused")
	}
}
