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

// machinePage is one machine's story: who has had it, what it was told to
// run and what came of it, and everything anybody did to it.
type machinePage struct {
	Device   repo.Device
	Report   *model.Status
	LastSync *time.Time
	Online   bool
	// TokenIssuedAt is when the machine's live device token was minted;
	// nil when it holds none (bucket channel, or revoked).
	TokenIssuedAt *time.Time
	Bindings      []bindingRow
	Targets       []machineTargetRow
	Events        []auditRow
	Versions      []machineVersion
}

// machineVersion is one product's line in the machine page's 版本 section:
// what it runs, what it is meant to run, and the choice between the fleet
// target and a version of its own.
type machineVersion struct {
	Product string
	Running string
	Global  string // the fleet target; "" when none is set
	Pinned  string // the version this machine is held to; "" = follows the fleet
	Choices []repo.Artifact
	// Why is set when this machine cannot be given a version of its own.
	Why string
}

type bindingRow struct {
	repo.Binding
	WindowsUser string
	Deleted     bool // the employee has since been deleted; the name is history
}

type machineTargetRow struct {
	repo.Target
	Version string
	Label   string
	Detail  string
}

func (s *Server) handleMachineDetail(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx := r.Context()
	device, err := s.dbm.store.Devices().ByHostname(ctx, strings.TrimSpace(r.URL.Query().Get("machine")))
	if err != nil {
		s.redirectWithError(w, r, "/overview", "no such machine")
		return
	}
	data := newPage(sess, r, "overview")
	page := &machinePage{Device: device}
	if at, err := s.dbm.store.DeviceTokens().IssuedAt(ctx, device.ID); err == nil {
		page.TokenIssuedAt = &at
	}

	if report, err := s.dbm.store.Reports().Get(ctx, device.ID); err == nil {
		var status model.Status
		if json.Unmarshal(report.Report, &status) == nil {
			page.Report = &status
		}
		page.LastSync = report.LastSyncAt
		page.Online = report.LastSyncAt != nil && time.Since(*report.LastSyncAt) <= onlineWithin
	} else if !errors.Is(err, repo.ErrNotFound) {
		data.Error = "could not read the machine's report"
	}

	if history, err := s.dbm.store.Bindings().History(ctx, device.ID); err == nil {
		for _, b := range history {
			row := bindingRow{Binding: b}
			if e, err := s.dbm.store.Employees().ByID(ctx, b.EmployeeID); err == nil {
				row.WindowsUser, row.Deleted = e.WindowsUser, e.Deleted()
			} else {
				row.WindowsUser = b.EmployeeID
			}
			page.Bindings = append(page.Bindings, row)
		}
	} else {
		data.Error = "could not read the binding history"
	}

	if targets, err := s.dbm.store.Releases().TargetsByDevice(ctx, device.ID); err == nil {
		now := time.Now()
		for _, t := range targets {
			artifact, err := s.dbm.store.Releases().ArtifactByID(ctx, t.ArtifactID)
			if err != nil {
				continue
			}
			label, detail := targetRowState(t, artifact, page.Report, page.LastSync, now)
			page.Targets = append(page.Targets, machineTargetRow{Target: t, Version: artifact.Version, Label: label, Detail: detail})
		}
	}

	page.Versions = s.machineVersions(r, device, page.Report)

	events, _, err := s.dbm.store.Audit().Search(ctx, repo.AuditFilter{TargetType: "device", TargetID: device.ID, Limit: 100})
	if err == nil {
		page.Events = s.auditRows(r, events)
	}
	data.MachinePage = page
	s.render(w, "machine.html", http.StatusOK, data)
}

// employeeHistory is the detail page's history in the database mode: every
// machine the person has held, and every recorded action on their account.
type employeeHistory struct {
	EmployeeID string
	Machines   []employeeMachineRow
	Events     []auditRow
	Usage      []usageLine // the last 30 days, oldest first
	UsageTotal usageLine
}

type employeeMachineRow struct {
	repo.Binding
	Hostname string
}

func (s *Server) employeeHistory(r *http.Request, windowsUser string) *employeeHistory {
	ctx := r.Context()
	e, err := s.dbm.store.Employees().ByWindowsUser(ctx, windowsUser)
	if err != nil {
		return nil
	}
	h := &employeeHistory{EmployeeID: e.ID}
	if bindings, err := s.dbm.store.Bindings().HistoryByEmployee(ctx, e.ID); err == nil {
		for _, b := range bindings {
			row := employeeMachineRow{Binding: b, Hostname: b.DeviceID}
			if d, err := s.dbm.store.Devices().ByID(ctx, b.DeviceID); err == nil {
				row.Hostname = d.Hostname
			}
			h.Machines = append(h.Machines, row)
		}
	}
	if events, _, err := s.dbm.store.Audit().Search(ctx, repo.AuditFilter{TargetType: "employee", TargetID: e.ID, Limit: 200}); err == nil {
		h.Events = s.auditRows(r, events)
	}
	h.Usage, h.UsageTotal = s.employeeUsage(r, e.ID)
	return h
}

func (s *Server) machineVersions(r *http.Request, device repo.Device, report *model.Status) []machineVersion {
	ctx := r.Context()
	pol, _, _ := s.dbm.ops.CurrentPolicy(ctx)
	all, _ := s.dbm.store.Releases().ListArtifacts(ctx, "")
	var out []machineVersion
	for _, product := range []string{repo.ProductAgent, repo.ProductCodex} {
		v := machineVersion{Product: product}
		if product == repo.ProductAgent {
			v.Global = pol.AgentUpdateVersion
			v.Running = device.AgentVersion
			if report != nil && report.AgentVersion != "" {
				v.Running = report.AgentVersion
			}
		} else {
			v.Global = pol.CodexVersion
			if report != nil {
				v.Running = report.CodexVersion
			}
		}
		target, err := s.dbm.store.Releases().OpenTarget(ctx, device.ID, product)
		if errors.Is(err, repo.ErrNotFound) {
			target, err = s.dbm.store.Releases().LastSucceededTarget(ctx, device.ID, product)
		}
		if err == nil {
			if a, err := s.dbm.store.Releases().ArtifactByID(ctx, target.ArtifactID); err == nil {
				v.Pinned = a.Version
			}
		}
		if !model.AgentCanTakeTargets(device.AgentVersion) {
			v.Why = fmt.Sprintf("agent %s 太旧,只能跟随全局;升到 %s 或更新后可单独指定", orDash(device.AgentVersion), model.MinTargetAgentVersion)
		}
		for _, a := range all {
			if a.Product == product && a.Status != repo.ArtifactRetired {
				v.Choices = append(v.Choices, a)
			}
		}
		out = append(out, v)
	}
	return out
}

// actionMachineVersion sets what one machine runs of a product: a version
// of its own (a one-machine rollout), or "" to follow the fleet target.
func (s *Server) actionMachineVersion(sess *session, r *http.Request) error {
	ctx := r.Context()
	device, err := s.dbm.store.Devices().ByHostname(ctx, formValue(r, "machine"))
	if err != nil {
		return err
	}
	product, artifactID := formValue(r, "product"), formValue(r, "artifact")
	if artifactID == "" {
		if err := s.dbm.ops.FollowGlobal(ctx, device.ID, product, sess.actor, s.clientKey(r)); err != nil {
			return err
		}
		logAudit(s.clientKey(r), "%s now follows the fleet %s target", device.Hostname, product)
		return nil
	}
	if _, err := s.dbm.ops.CreateRollout(ctx, ops.RolloutSpec{
		Product: product, ArtifactID: artifactID, DeviceIDs: []string{device.ID},
		Kind: repo.RolloutRelease, Note: "机器页指定", Actor: sess.actor, RequestID: s.clientKey(r),
	}); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "pinned %s to %s %s", device.Hostname, product, artifactID)
	return nil
}

// backToMachine sends a machine-page action back to that machine.
func backToMachine(r *http.Request) string {
	return "/machines/detail?machine=" + url.QueryEscape(r.PostFormValue("machine"))
}
