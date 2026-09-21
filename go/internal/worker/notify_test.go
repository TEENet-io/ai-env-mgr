package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/notify"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type fakeChannel struct {
	name string
	fail error
	got  []notify.Message
}

func (c *fakeChannel) Name() string { return c.name }
func (c *fakeChannel) Send(_ context.Context, m notify.Message) error {
	if c.fail != nil {
		return c.fail
	}
	c.got = append(c.got, m)
	return nil
}

func TestAlertNotifyDeliversOnceAndRecordsFailures(t *testing.T) {
	store, ctx := newWorkerStore(t)
	a1, _, _ := store.Alerts().Open(ctx, repo.NewAlert{Kind: repo.AlertMachineOffline, Fingerprint: "m:1", Severity: "warn", SubjectType: "device", SubjectID: "d1", Title: "PC-1 未上报"})
	a2, _, _ := store.Alerts().Open(ctx, repo.NewAlert{Kind: repo.AlertBudget, Fingerprint: "b:1", Severity: "crit", SubjectType: "employee", SubjectID: "e1", Title: "work1 超预算"})
	hook := &fakeChannel{name: "webhook"}
	mail := &fakeChannel{name: "smtp", fail: errors.New("connection refused")}
	channels := []notify.Channel{hook, mail}
	now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	h := AlertNotify{Store: store, BaseURL: "https://console.example/", Now: func() time.Time { return now },
		Channels: func(context.Context) ([]notify.Channel, error) { return channels, nil }}

	res, err := h.Run(ctx, repo.Task{})
	if err != nil || res.Note != "sent 0, 2 failed (will retry)" {
		t.Fatalf("with the relay down: %q %v", res.Note, err)
	}
	if len(hook.got) != 2 || hook.got[0].URL != "https://console.example/alerts" {
		t.Fatalf("the webhook still gets the message: %+v", hook.got)
	}
	a, _ := store.Alerts().ByID(ctx, a1.ID)
	if a.NotifiedAt != nil || a.NotifyTries != 1 || !strings.Contains(a.NotifyError, "smtp: connection refused") {
		t.Fatalf("after a failed send: %+v", a)
	}

	// The relay comes back: both go out, and nothing goes out twice after.
	mail.fail = nil
	if res, _ = h.Run(ctx, repo.Task{}); res.Note != "sent 2" {
		t.Fatalf("relay back: %q", res.Note)
	}
	if res, _ = h.Run(ctx, repo.Task{}); res.Note != "nothing waiting" {
		t.Fatalf("third run: %q", res.Note)
	}
	if len(hook.got) != 4 || len(mail.got) != 2 {
		t.Fatalf("webhook %d, mail %d", len(hook.got), len(mail.got))
	}
	a, _ = store.Alerts().ByID(ctx, a2.ID)
	if a.NotifiedAt == nil || a.NotifyError != "" {
		t.Fatalf("after success: %+v", a)
	}

	// A new alert with nowhere to go is marked seen rather than kept
	// forever; a channel configured later does not replay it.
	channels = nil
	a3, _, _ := store.Alerts().Open(ctx, repo.NewAlert{Kind: repo.AlertTaskFailed, Fingerprint: "t:1", Severity: "warn", SubjectType: "task", SubjectID: "t1", Title: "任务失败"})
	if res, _ = h.Run(ctx, repo.Task{}); !strings.Contains(res.Note, "no channel configured; 1 alert") {
		t.Fatalf("no channels: %q", res.Note)
	}
	if a, _ = store.Alerts().ByID(ctx, a3.ID); a.NotifiedAt == nil {
		t.Fatal("unsendable alert was left waiting")
	}

	// After MaxTries the alert is left with its error.
	channels = []notify.Channel{&fakeChannel{name: "webhook", fail: errors.New("410 gone")}}
	h.MaxTries = 2
	a4, _, _ := store.Alerts().Open(ctx, repo.NewAlert{Kind: repo.AlertTaskFailed, Fingerprint: "t:2", Severity: "warn", SubjectType: "task", SubjectID: "t2", Title: "任务失败"})
	for range 5 {
		h.Run(ctx, repo.Task{})
	}
	if a, _ = store.Alerts().ByID(ctx, a4.ID); a.NotifyTries != 3 || a.NotifiedAt != nil {
		t.Fatalf("after giving up: %+v", a)
	}
}
