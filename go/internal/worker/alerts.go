package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// UserLister is the part of the gateway the budget rule reads.
type UserLister interface {
	ListUsers(ctx context.Context) ([]litellm.User, error)
}

// AlertEval runs the rules and keeps the alert table in step with what they
// see: a condition that is there and not yet open is opened, one that is
// open and no longer there is resolved. Nothing is notified here; delivery
// is the notify task's, which reads what this leaves open.
//
// Two kinds are opened elsewhere, by the event that finds them -- a
// reconciliation that sees drift, a task that runs out of retries -- and
// this only closes them once the state has moved on.
type AlertEval struct {
	Store   repo.Store
	Gateway UserLister // nil: the budget rule is skipped
	Now     func() time.Time
}

// Run evaluates every rule. A rule that cannot read its inputs is skipped
// and named in the note; the others still run, and none of the skipped
// rule's alerts are resolved on missing data.
func (h AlertEval) Run(ctx context.Context, _ repo.Task) (Result, error) {
	settings, _, err := repo.LoadAlertSettings(ctx, h.Store.Settings())
	if err != nil {
		return Result{}, err
	}
	if !settings.Enabled {
		return Result{Note: "alerts are off"}, nil
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	open, err := h.Store.Alerts().ListOpen(ctx)
	if err != nil {
		return Result{}, err
	}
	var opened, resolved int
	var skipped []string
	apply := func(kind string, want []repo.NewAlert, err error) {
		if err != nil {
			skipped = append(skipped, kind+": "+err.Error())
			return
		}
		o, r, err := h.reconcile(ctx, kind, open, want)
		if err != nil {
			skipped = append(skipped, kind+": "+err.Error())
			return
		}
		opened, resolved = opened+o, resolved+r
	}
	want, err := h.offlineMachines(ctx, now, settings)
	apply(repo.AlertMachineOffline, want, err)
	want, err = h.failedRollouts(ctx, now)
	apply(repo.AlertRolloutFailed, want, err)
	want, err = h.offboardCleanup(ctx, now, settings)
	apply(repo.AlertOffboardCleanup, want, err)
	if h.Gateway != nil {
		want, err = h.budgets(ctx, settings)
		apply(repo.AlertBudget, want, err)
	}
	r, err := h.closeSettledDrift(ctx, open)
	if err != nil {
		skipped = append(skipped, repo.AlertGatewayDrift+": "+err.Error())
	}
	resolved += r
	r, err = h.closeRevivedTasks(ctx, open)
	if err != nil {
		skipped = append(skipped, repo.AlertTaskFailed+": "+err.Error())
	}
	resolved += r

	note := fmt.Sprintf("opened %d, resolved %d", opened, resolved)
	if len(skipped) > 0 {
		note += "; skipped " + strings.Join(skipped, "; ")
	}
	return Result{Note: note}, nil
}

// reconcile opens what is wanted and not open, and resolves what is open
// for this kind and not wanted.
func (h AlertEval) reconcile(ctx context.Context, kind string, open []repo.Alert, want []repo.NewAlert) (opened, resolved int, err error) {
	wanted := map[string]bool{}
	for _, n := range want {
		wanted[n.Fingerprint] = true
		_, created, err := h.Store.Alerts().Open(ctx, n)
		if err != nil {
			return opened, resolved, err
		}
		if created {
			opened++
		}
	}
	for _, a := range open {
		if a.Kind != kind || wanted[a.Fingerprint] {
			continue
		}
		n, err := h.Store.Alerts().Resolve(ctx, a.Fingerprint, "rule")
		if err != nil {
			return opened, resolved, err
		}
		resolved += n
	}
	return opened, resolved, nil
}

// offlineMachines: a machine with an employee on it that has not reported
// for longer than the threshold. Unbound machines are somebody's spare and
// are not anybody's problem yet. Neither is a machine whose employee has
// left: offboarding keeps the binding (it is how the revocation reaches the
// desktop) and the instance is shut down the same day, so its silence is
// the process working, not a fault.
func (h AlertEval) offlineMachines(ctx context.Context, now time.Time, s repo.AlertSettings) ([]repo.NewAlert, error) {
	bindings, err := h.Store.Bindings().ListOpen(ctx)
	if err != nil {
		return nil, err
	}
	limit := now.Add(-time.Duration(s.OfflineAfterHours) * time.Hour)
	var out []repo.NewAlert
	for _, b := range bindings {
		d, err := h.Store.Devices().ByID(ctx, b.DeviceID)
		if err != nil {
			return nil, err
		}
		if d.LastSeenAt != nil && d.LastSeenAt.After(limit) {
			continue
		}
		if e, err := h.Store.Employees().ByID(ctx, b.EmployeeID); err != nil {
			return nil, err
		} else if !e.Active() {
			continue
		}
		title := fmt.Sprintf("机器 %s 从未上报", d.Hostname)
		detail := ""
		if d.LastSeenAt != nil {
			title = fmt.Sprintf("机器 %s 已 %d 小时未上报", d.Hostname, int(now.Sub(*d.LastSeenAt).Hours()))
			detail = "最后上报 " + d.LastSeenAt.Local().Format("2006-01-02 15:04")
		}
		out = append(out, repo.NewAlert{
			Kind: repo.AlertMachineOffline, Fingerprint: repo.AlertMachineOffline + ":" + d.ID,
			Severity: repo.SeverityWarn, SubjectType: "device", SubjectID: d.ID,
			Title: title, Detail: detail,
		})
	}
	return out, nil
}

// offboardCleanup: an employee who left at least CleanupAfterDays ago and
// still has a machine in the console. The process (ai工作间流程 §8) deletes
// the cloud account and releases the instance then; the console's part is
// to forget the machine, and forgetting it is what closes the alert. An
// employee who never had a machine here has nothing for the console to
// track, and is not reported.
func (h AlertEval) offboardCleanup(ctx context.Context, now time.Time, s repo.AlertSettings) ([]repo.NewAlert, error) {
	employees, err := h.Store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		return nil, err
	}
	due := now.Add(-time.Duration(s.CleanupAfterDays) * 24 * time.Hour)
	var out []repo.NewAlert
	for _, e := range employees {
		if e.Status != repo.StatusOffboarded || e.OffboardedAt == nil || e.OffboardedAt.After(due) {
			continue
		}
		bindings, err := h.Store.Bindings().OpenByEmployee(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		var machines []string
		for _, b := range bindings {
			d, err := h.Store.Devices().ByID(ctx, b.DeviceID)
			if err != nil {
				return nil, err
			}
			if d.Status != repo.DeviceRevoked {
				machines = append(machines, d.Hostname)
			}
		}
		if len(machines) == 0 {
			continue
		}
		days := int(now.Sub(*e.OffboardedAt).Hours() / 24)
		out = append(out, repo.NewAlert{
			Kind: repo.AlertOffboardCleanup, Fingerprint: repo.AlertOffboardCleanup + ":" + e.ID,
			Severity: repo.SeverityWarn, SubjectType: "employee", SubjectID: e.ID,
			Title: fmt.Sprintf("%s 已离职 %d 天，待清理", e.WindowsUser, days),
			Detail: fmt.Sprintf("关户于 %s。按流程删除阿里云账号、释放实例并删除数据，然后在机器页「注销」%s；注销后这条自动关闭。",
				e.OffboardedAt.Local().Format("2006-01-02"), strings.Join(machines, "、")),
		})
	}
	return out, nil
}

// failedRollouts: a machine whose latest target for a product failed. An
// older generation that failed and was retried is history, not a condition.
func (h AlertEval) failedRollouts(ctx context.Context, now time.Time) ([]repo.NewAlert, error) {
	targets, err := h.Store.Releases().TargetsSince(ctx, now.AddDate(0, 0, -90))
	if err != nil {
		return nil, err
	}
	latest := map[string]repo.Target{}
	for _, t := range targets {
		key := t.DeviceID + "/" + t.Product
		if cur, ok := latest[key]; !ok || t.Generation > cur.Generation {
			latest[key] = t
		}
	}
	var out []repo.NewAlert
	for _, t := range latest {
		if t.Status != repo.TargetFailed {
			continue
		}
		host := t.DeviceID
		if d, err := h.Store.Devices().ByID(ctx, t.DeviceID); err == nil {
			host = d.Hostname
		}
		version := ""
		if a, err := h.Store.Releases().ArtifactByID(ctx, t.ArtifactID); err == nil {
			version = a.Version
		}
		out = append(out, repo.NewAlert{
			Kind: repo.AlertRolloutFailed, Fingerprint: repo.AlertRolloutFailed + ":" + t.ID,
			Severity: repo.SeverityWarn, SubjectType: "target", SubjectID: t.ID,
			Title: fmt.Sprintf("机器 %s 安装 %s %s 失败", host, t.Product, version), Detail: t.ResultNote,
		})
	}
	return out, nil
}

// budgets: spend against the gateway's own monthly budget per user. The
// gateway resets spend on the first of the month, which is what closes
// these without anybody doing anything.
func (h AlertEval) budgets(ctx context.Context, s repo.AlertSettings) ([]repo.NewAlert, error) {
	users, err := h.Gateway.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []repo.NewAlert
	for _, u := range users {
		if u.MaxBudget == nil || *u.MaxBudget <= 0 {
			continue
		}
		windowsUser, ok := strings.CutPrefix(u.UserID, "emp-")
		if !ok {
			continue
		}
		pct := int(u.Spend / *u.MaxBudget * 100)
		if pct < s.BudgetWarnPercent {
			continue
		}
		subject := u.UserID
		if e, err := h.Store.Employees().ByWindowsUser(ctx, windowsUser); err == nil {
			subject = e.ID
		} else if !errors.Is(err, repo.ErrNotFound) {
			return nil, err
		}
		level, severity := "warn", repo.SeverityWarn
		if pct >= 100 {
			level, severity = "crit", repo.SeverityCrit
		}
		out = append(out, repo.NewAlert{
			Kind: repo.AlertBudget, Fingerprint: fmt.Sprintf("%s:%s:%s", repo.AlertBudget, subject, level),
			Severity: severity, SubjectType: "employee", SubjectID: subject,
			Title:  fmt.Sprintf("%s 本月已用预算 %d%%", windowsUser, pct),
			Detail: fmt.Sprintf("已用 $%.2f / $%.2f", u.Spend, *u.MaxBudget),
		})
	}
	return out, nil
}

// closeSettledDrift resolves drift alerts whose grant now reads as intended.
func (h AlertEval) closeSettledDrift(ctx context.Context, open []repo.Alert) (int, error) {
	resolved := 0
	for _, a := range open {
		if a.Kind != repo.AlertGatewayDrift {
			continue
		}
		g, err := h.Store.Grants().ByKeyAlias(ctx, "", a.SubjectID)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			return resolved, err
		}
		if !grantSettled(g) {
			continue
		}
		n, err := h.Store.Alerts().Resolve(ctx, a.Fingerprint, "rule")
		if err != nil {
			return resolved, err
		}
		resolved += n
	}
	return resolved, nil
}

// grantSettled is "the gateway holds what we intend": an active grant with
// an active key, or a revoked one whose key is gone.
func grantSettled(g repo.Grant) bool {
	switch g.Desired {
	case repo.GrantActive:
		return g.Actual == repo.ActualActive
	case repo.GrantRevoked:
		return g.Actual == repo.ActualRevoked || g.Actual == repo.ActualMissing
	}
	return false
}

// closeRevivedTasks resolves task alerts once the task is no longer failed:
// retried from the console, or superseded by newer work.
func (h AlertEval) closeRevivedTasks(ctx context.Context, open []repo.Alert) (int, error) {
	resolved := 0
	for _, a := range open {
		if a.Kind != repo.AlertTaskFailed {
			continue
		}
		t, err := h.Store.Tasks().ByID(ctx, a.SubjectID)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return resolved, err
		}
		if err == nil && t.Status == repo.TaskFailed {
			continue
		}
		n, err := h.Store.Alerts().Resolve(ctx, a.Fingerprint, "rule")
		if err != nil {
			return resolved, err
		}
		resolved += n
	}
	return resolved, nil
}

// OpenDriftAlert is the reconciliation's hook: the gateway holds something
// other than what the console intends for this token.
func OpenDriftAlert(ctx context.Context, store repo.Store, grant repo.Grant, observed string) error {
	_, _, err := store.Alerts().Open(ctx, repo.NewAlert{
		Kind: repo.AlertGatewayDrift, Fingerprint: repo.AlertGatewayDrift + ":" + grant.KeyAlias,
		Severity: repo.SeverityCrit, SubjectType: "grant", SubjectID: grant.KeyAlias,
		Title:  fmt.Sprintf("网关令牌 %s 状态不符", grant.KeyAlias),
		Detail: fmt.Sprintf("控制台认为 %s，网关实际 %s", grant.Desired, observed),
	})
	return err
}

// OpenTaskFailedAlert is the worker's hook for a task that has run out of
// retries, or was refused outright.
func OpenTaskFailedAlert(ctx context.Context, store repo.Store, task repo.Task, cause error) error {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	_, _, err := store.Alerts().Open(ctx, repo.NewAlert{
		Kind: repo.AlertTaskFailed, Fingerprint: repo.AlertTaskFailed + ":" + task.ID,
		Severity: repo.SeverityWarn, SubjectType: "task", SubjectID: task.ID,
		Title:  fmt.Sprintf("任务 %s 已放弃（%d 次尝试）", task.Kind, task.Attempts),
		Detail: detail,
	})
	return err
}

// EnqueueAlertEval queues an evaluation now, for the settings page after a
// threshold change.
func EnqueueAlertEval(ctx context.Context, store repo.Store, at time.Time) error {
	_, _, err := store.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           TaskAlertEval,
		IdempotencyKey: TaskAlertEval + ":manual:" + at.UTC().Format(time.RFC3339),
		MaxAttempts:    2,
	})
	return err
}
