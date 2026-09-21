package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/notify"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// AlertNotify tells the channels about open alerts nobody has been told
// about. Each alert is sent to every channel; it is marked notified only
// when all of them took it, so a relay that was down does not cost the
// message, and a webhook that was up is asked again. After MaxTries the
// alert is left alone with its last error on it, visible on the page.
type AlertNotify struct {
	Store repo.Store
	// Channels is read on every run: settings change without a restart.
	Channels func(ctx context.Context) ([]notify.Channel, error)
	// BaseURL is the console's address, put in each message; may be empty.
	BaseURL  string
	MaxTries int // default 10
	Now      func() time.Time
}

// Run sends what is waiting. A run only fails when the channels cannot be
// read; a channel that refuses is recorded on the alert and retried.
func (h AlertNotify) Run(ctx context.Context, _ repo.Task) (Result, error) {
	maxTries := h.MaxTries
	if maxTries <= 0 {
		maxTries = 10
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	waiting, err := h.Store.Alerts().Unnotified(ctx, maxTries, 50)
	if err != nil {
		return Result{}, err
	}
	if len(waiting) == 0 {
		return Result{Note: "nothing waiting"}, nil
	}
	channels, err := h.Channels(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(channels) == 0 {
		// With nowhere to send, what is waiting is marked seen: configuring
		// a channel next month must not replay this month's alerts.
		for _, a := range waiting {
			if err := h.Store.Alerts().MarkNotified(ctx, a.ID, now, ""); err != nil {
				return Result{}, err
			}
		}
		return Result{Note: fmt.Sprintf("no channel configured; %d alert(s) marked as seen", len(waiting))}, nil
	}
	sent, failed := 0, 0
	for _, a := range waiting {
		m := notify.Message{Title: a.Title, Severity: a.Severity, Subject: a.SubjectType + " " + a.SubjectID, Detail: a.Detail, At: a.OpenedAt}
		if h.BaseURL != "" {
			m.URL = strings.TrimSuffix(h.BaseURL, "/") + "/alerts"
		}
		var errs []string
		for _, ch := range channels {
			if err := ch.Send(ctx, m); err != nil {
				errs = append(errs, ch.Name()+": "+err.Error())
			}
		}
		if len(errs) == 0 {
			if err := h.Store.Alerts().MarkNotified(ctx, a.ID, now, ""); err != nil {
				return Result{}, err
			}
			sent++
			continue
		}
		failed++
		if err := h.Store.Alerts().MarkNotified(ctx, a.ID, time.Time{}, strings.Join(errs, "; ")); err != nil {
			return Result{}, err
		}
	}
	note := fmt.Sprintf("sent %d", sent)
	if failed > 0 {
		note += fmt.Sprintf(", %d failed (will retry)", failed)
	}
	return Result{Note: note}, nil
}
