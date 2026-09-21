package adminweb

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// machinePage is one machine's story: who has had it, what it was told to
// run and what came of it, and everything anybody did to it.
type machinePage struct {
	Device   repo.Device
	Report   *model.Status
	LastSync *time.Time
	Online   bool
	Bindings []bindingRow
	Targets  []machineTargetRow
	Events   []auditRow
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
