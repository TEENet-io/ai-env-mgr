package adminweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// onlineWithin is how recent a report has to be for a machine to count as
// reachable on this page. Agents heartbeat every few minutes.
const onlineWithin = 15 * time.Minute

// targetRowState says what one machine is doing about its target, from the
// target row and the machine's own report. It reads; it never decides --
// settlement happens in the worker, and this label must agree with it.
func targetRowState(t repo.Target, artifact repo.Artifact, report *model.Status, lastSync *time.Time, now time.Time) (label, detail string) {
	switch t.Status {
	case repo.TargetSucceeded:
		return "成功", "实际版本 " + t.ReportedVersion
	case repo.TargetFailed:
		return "失败", t.ResultNote
	case repo.TargetExcluded:
		return "已排除", t.ExcludeReason
	case repo.TargetCancelled:
		return "已取消", ""
	case repo.TargetSuperseded:
		return "已取代", "已有更新的目标"
	}
	online := lastSync != nil && now.Sub(*lastSync) <= onlineWithin
	waiting := "待执行(离线)"
	if online {
		waiting = "待执行(在线)"
	}
	if report == nil || lastSync == nil {
		return waiting, "从未上报"
	}
	seen := "最后联系 " + lastSync.Format("01-02 15:04")
	running, seenTarget, seenGen, state, reason := reportFor(t.Product, report)
	if running == artifact.Version && lastSync.After(t.CreatedAt) {
		return "待健康确认", "已是目标版本,等待结算"
	}
	if seenTarget != artifact.Version || seenGen != t.Generation {
		return waiting, seen
	}
	switch state {
	case "deferred":
		switch reason {
		case "in_use":
			return "延后:正在使用", seen
		case "disk":
			return "延后:磁盘不足", seen
		default:
			return "延后", reason
		}
	case "downloading":
		return "下载中", seen
	case "installing":
		return "安装中", seen
	case "pending":
		return "下载中", "已校验,重启中"
	}
	return waiting, seen
}

func reportFor(product string, s *model.Status) (running, target string, generation int, state, reason string) {
	if product == repo.ProductAgent {
		return s.AgentVersion, s.AgentUpdateTarget, s.AgentUpdateGeneration, s.AgentUpdateState, ""
	}
	return s.CodexVersion, s.CodexTarget, s.CodexTargetGeneration, s.CodexState, s.CodexDeferReason
}

type targetSummary struct{ Total, Pending, Succeeded, Failed, Excluded, Cancelled, Superseded int }

// Complete is the spec's condition: every machine succeeded, or was
// explicitly excluded with a reason. Excluded is not success and is not
// counted as it; a rollout whose targets were all taken over by a newer one
// is not complete either, it is history.
func (s targetSummary) Complete() bool {
	return s.Total > 0 && s.Pending == 0 && s.Failed == 0 && s.Cancelled == 0 && s.Superseded == 0
}

// summariseTargets counts one outcome per machine: its latest attempt in the
// rollout. A machine that failed and then succeeded on a retry is one
// success, not one failure and one success; the earlier rows stay in the
// table as history.
func summariseTargets(targets []repo.Target) targetSummary {
	latest := map[string]repo.Target{}
	for _, t := range targets {
		if have, ok := latest[t.DeviceID]; !ok || t.Generation > have.Generation {
			latest[t.DeviceID] = t
		}
	}
	var s targetSummary
	for _, t := range latest {
		s.Total++
		switch t.Status {
		case repo.TargetPending:
			s.Pending++
		case repo.TargetSucceeded:
			s.Succeeded++
		case repo.TargetFailed:
			s.Failed++
		case repo.TargetExcluded:
			s.Excluded++
		case repo.TargetCancelled:
			s.Cancelled++
		case repo.TargetSuperseded:
			s.Superseded++
		}
	}
	return s
}

// deviceCandidate is one machine on the "new rollout" page.
type deviceCandidate struct {
	repo.Device
	CodexVersion string
	LastSync     *time.Time
	Online       bool
	Compatible   bool
	Why          string // why not compatible
	HasOpen      bool   // an open target for this product already
}

// targetRow is one machine on a rollout's page.
type targetRow struct {
	repo.Target
	Hostname string
	Label    string
	Detail   string
}

// rolloutView is a rollout with what the page shows about it.
type rolloutView struct {
	repo.Rollout
	Artifact repo.Artifact
	Summary  targetSummary
	Targets  []targetRow
	Paused   bool
	// Rollbacks are the other artifacts of the same product, offered on the
	// detail page as what to go back to.
	Rollbacks []repo.Artifact
}

func (s *Server) rolloutView(r *http.Request, rollout repo.Rollout, withTargets bool) (rolloutView, error) {
	ctx := r.Context()
	artifact, err := s.dbm.store.Releases().ArtifactByID(ctx, rollout.ArtifactID)
	if err != nil {
		return rolloutView{}, err
	}
	targets, err := s.dbm.store.Releases().TargetsByRollout(ctx, rollout.ID)
	if err != nil {
		return rolloutView{}, err
	}
	view := rolloutView{Rollout: rollout, Artifact: artifact, Summary: summariseTargets(targets), Paused: rollout.PausedAt != nil}
	if !withTargets {
		return view, nil
	}
	now := time.Now()
	for _, t := range targets {
		device, err := s.dbm.store.Devices().ByID(ctx, t.DeviceID)
		if err != nil {
			return rolloutView{}, err
		}
		var status *model.Status
		var lastSync *time.Time
		report, err := s.dbm.store.Reports().Get(ctx, t.DeviceID)
		switch {
		case err == nil:
			var parsed model.Status
			if json.Unmarshal(report.Report, &parsed) == nil {
				status = &parsed
			}
			lastSync = report.LastSyncAt
		case !errors.Is(err, repo.ErrNotFound):
			return rolloutView{}, err
		}
		label, detail := targetRowState(t, artifact, status, lastSync, now)
		view.Targets = append(view.Targets, targetRow{Target: t, Hostname: device.Hostname, Label: label, Detail: detail})
	}
	all, err := s.dbm.store.Releases().ListArtifacts(ctx, rollout.Product)
	if err != nil {
		return rolloutView{}, err
	}
	for _, a := range all {
		if a.ID != artifact.ID && a.Status != repo.ArtifactRetired {
			view.Rollbacks = append(view.Rollbacks, a)
		}
	}
	return view, nil
}

func (s *Server) handleRollouts(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "releases")
	rollouts, err := s.dbm.store.Releases().ListRollouts(r.Context(), 50)
	if err != nil {
		data.Error = "could not list rollouts"
	}
	for _, ro := range rollouts {
		view, err := s.rolloutView(r, ro, false)
		if err != nil {
			data.Error = "could not read a rollout: " + err.Error()
			break
		}
		data.Rollouts = append(data.Rollouts, view)
	}
	s.render(w, "rollouts.html", http.StatusOK, data)
}

func (s *Server) handleRolloutNew(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx := r.Context()
	artifact, err := s.dbm.store.Releases().ArtifactByID(ctx, strings.TrimSpace(r.URL.Query().Get("artifact")))
	if err != nil {
		s.redirectWithError(w, r, "/releases", "choose a version from the library first")
		return
	}
	data := newPage(sess, r, "releases")
	data.Artifact = &artifact
	devices, err := s.dbm.store.Devices().List(ctx, repo.DeviceFilter{})
	if err != nil {
		data.Error = "could not list machines"
	}
	reports, _ := s.dbm.store.Reports().List(ctx)
	reportOf := map[string]repo.DeviceReport{}
	for _, rep := range reports {
		reportOf[rep.DeviceID] = rep
	}
	now := time.Now()
	for _, d := range devices {
		c := deviceCandidate{Device: d}
		if rep, ok := reportOf[d.ID]; ok {
			c.CodexVersion = rep.CodexVersion
			c.LastSync = rep.LastSyncAt
			c.Online = rep.LastSyncAt != nil && now.Sub(*rep.LastSyncAt) <= onlineWithin
		}
		switch {
		case !model.AgentCanTakeTargets(d.AgentVersion):
			c.Why = fmt.Sprintf("agent %s 只读全局策略,需要 %s 或更新", orDash(d.AgentVersion), model.MinTargetAgentVersion)
		case artifact.MinAgentVersion != "" && model.CompareVersions(d.AgentVersion, artifact.MinAgentVersion) < 0:
			c.Why = fmt.Sprintf("agent %s 低于该版本要求的 %s", d.AgentVersion, artifact.MinAgentVersion)
		default:
			c.Compatible = true
		}
		if _, err := s.dbm.store.Releases().OpenTarget(ctx, d.ID, artifact.Product); err == nil {
			c.HasOpen = true
		}
		data.Candidates = append(data.Candidates, c)
	}
	s.render(w, "rollout_new.html", http.StatusOK, data)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (s *Server) handleRolloutDetail(w http.ResponseWriter, r *http.Request, sess *session) {
	rollout, err := s.dbm.store.Releases().RolloutByID(r.Context(), strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		s.redirectWithError(w, r, "/rollouts", "no such rollout")
		return
	}
	data := newPage(sess, r, "releases")
	view, err := s.rolloutView(r, rollout, true)
	if err != nil {
		data.Error = "could not read the rollout: " + err.Error()
	}
	data.Rollout = &view
	s.render(w, "rollout_detail.html", http.StatusOK, data)
}

// backToRollout sends an action back to the rollout it was about.
func backToRollout(r *http.Request) string {
	if id := r.PostFormValue("id"); id != "" {
		return "/rollouts/detail?id=" + url.QueryEscape(id)
	}
	return "/rollouts"
}

func (s *Server) actionRolloutCreate(sess *session, r *http.Request) error {
	kind := repo.RolloutKind(formValue(r, "kind"))
	if kind == "" {
		kind = repo.RolloutRelease
	}
	_, err := s.dbm.ops.CreateRollout(r.Context(), ops.RolloutSpec{
		Product: formValue(r, "product"), ArtifactID: formValue(r, "artifact"),
		DeviceIDs: r.PostForm["device"], Kind: kind, RollbackOf: formValue(r, "rollback_of"),
		Note: formValue(r, "note"), Actor: sess.actor, RequestID: s.clientKey(r),
	})
	if err != nil {
		return err
	}
	logAudit(s.clientKey(r), "created a %s rollout of %s to %d machine(s)", kind, formValue(r, "artifact"), len(r.PostForm["device"]))
	return nil
}

func (s *Server) actionRolloutPause(sess *session, r *http.Request) error {
	return s.dbm.ops.SetRolloutPaused(r.Context(), formValue(r, "id"), true, sess.actor, s.clientKey(r))
}

func (s *Server) actionRolloutResume(sess *session, r *http.Request) error {
	return s.dbm.ops.SetRolloutPaused(r.Context(), formValue(r, "id"), false, sess.actor, s.clientKey(r))
}

func (s *Server) actionRolloutCancel(sess *session, r *http.Request) error {
	_, err := s.dbm.ops.CancelRollout(r.Context(), formValue(r, "id"), sess.actor, s.clientKey(r))
	return err
}

func (s *Server) actionRolloutExclude(sess *session, r *http.Request) error {
	return s.dbm.ops.ExcludeTarget(r.Context(), formValue(r, "target"), formValue(r, "reason"), sess.actor, s.clientKey(r))
}

func (s *Server) actionRolloutRetry(sess *session, r *http.Request) error {
	_, err := s.dbm.ops.RetryTarget(r.Context(), formValue(r, "target"), sess.actor, s.clientKey(r))
	return err
}

// actionRolloutRollback opens a new rollout of an older artifact to the
// machines this rollout updated. The original stays as it is: history.
func (s *Server) actionRolloutRollback(sess *session, r *http.Request) error {
	ctx := r.Context()
	rollout, err := s.dbm.store.Releases().RolloutByID(ctx, formValue(r, "id"))
	if err != nil {
		return err
	}
	artifact, err := s.dbm.store.Releases().ArtifactByID(ctx, formValue(r, "artifact"))
	if err != nil {
		return err
	}
	if err := confirmMatches(r, "confirm", artifact.Version); err != nil {
		return err
	}
	targets, err := s.dbm.store.Releases().TargetsByRollout(ctx, rollout.ID)
	if err != nil {
		return err
	}
	var devices []string
	for _, t := range targets {
		if t.Status == repo.TargetSucceeded {
			devices = append(devices, t.DeviceID)
		}
	}
	if len(devices) == 0 {
		return fmt.Errorf("no machine in this rollout has taken the version; there is nothing to roll back")
	}
	_, err = s.dbm.ops.CreateRollout(ctx, ops.RolloutSpec{
		Product: rollout.Product, ArtifactID: artifact.ID, DeviceIDs: devices,
		Kind: repo.RolloutRollback, RollbackOf: rollout.ID, Note: "回滚 " + formValue(r, "note"),
		Actor: sess.actor, RequestID: s.clientKey(r),
	})
	if err != nil {
		return err
	}
	logAudit(s.clientKey(r), "rolled back rollout %s to %s %s on %d machine(s)", rollout.ID, artifact.Product, artifact.Version, len(devices))
	return nil
}
