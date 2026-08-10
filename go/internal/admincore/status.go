package admincore

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/status"
)

// administratorAccount is the one local account every Windows machine has
// that is never a mistake — it never needs flagging as an "extra" user.
const administratorAccount = "administrator"

// MachineState is one machine's status as seen by the admin: its self-report
// (if any), its binding (if any), and the discrepancies between the two.
type MachineState struct {
	Machine string
	Status  model.Status
	Binding model.Binding
	Bound   bool

	Stale       bool     // hasn't reported within the requested threshold
	Missing     bool     // bound, but has never reported at all
	Unbound     bool     // has reported, but nobody has assigned it to an employee
	UserMissing bool     // the bound employee has no profile on the machine
	Disabled    bool     // bound to an employee who has been offboarded
	ExtraUsers  []string // local profiles that are neither the bound employee nor Administrator
}

// CollectMachines assembles the admin's machine-centric status view.
//
// Machines are the unit of tracking, not employees, because the binding
// (employee -> machine) and the report (machine -> what it's doing) are two
// independent objects in OSS that can each exist without the other: a
// machine can be bound before the agent ever runs (Missing), and an agent
// can report before anyone binds it (Unbound, e.g. right after imaging). The
// two prefixes are unioned rather than one driving the other so both
// situations stay visible instead of one silently hiding the other.
func (m *Manager) CollectMachines(staleAfter time.Duration) ([]MachineState, error) {
	statusKeys, err := m.Store.List(ossclient.StatusPrefix)
	if err != nil {
		return nil, fmt.Errorf("list status reports: %w", err)
	}
	bindingKeys, err := m.Store.List(ossclient.BindingPrefix)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}

	// The roster tells us which bound employees have been offboarded. A
	// machine still bound to one is a loose end -- it is still running, still
	// reporting, and someone needs to reassign or decommission it -- so it is
	// flagged rather than dropped from the view. Dropping it is how a machine
	// gets forgotten while still switched on.
	roster, err := m.LoadUsers()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(statusKeys)+len(bindingKeys))
	for _, k := range statusKeys {
		seen[ossclient.MachineFromStatusKey(k)] = struct{}{}
	}
	for _, k := range bindingKeys {
		seen[ossclient.MachineFromBindingKey(k)] = struct{}{}
	}

	machines := make([]string, 0, len(seen))
	for machine := range seen {
		machines = append(machines, machine)
	}
	sort.Strings(machines)

	states := make([]MachineState, 0, len(machines))
	for _, machine := range machines {
		st, hasStatus := m.loadMachineStatus(machine)

		binding, bound, err := m.LoadBinding(machine)
		if err != nil {
			return nil, err
		}

		state := MachineState{
			Machine: machine,
			Status:  st,
			Binding: binding,
			Bound:   bound,
			Missing: bound && !hasStatus,
			Unbound: !bound && hasStatus,
		}

		// A machine that has never reported has no timestamp to trust, so it
		// is treated as stale the same way agentcore treats a corrupt report:
		// silence must never be mistaken for health.
		if hasStatus {
			state.Stale = status.Age(st) > staleAfter
		} else {
			state.Stale = true
		}

		if bound {
			if e := roster.Find(binding.User); e != nil && !e.Enabled {
				state.Disabled = true
			}
		}
		if bound && hasStatus {
			state.UserMissing = !st.HasLocalUser(binding.User)
		}
		state.ExtraUsers = extraLocalUsers(st.LocalUsers, binding.User, bound)

		states = append(states, state)
	}
	return states, nil
}

// loadMachineStatus reads one machine's status report. A missing or corrupt
// report is reported back as "no status" rather than an error: it cannot be
// told apart from "never synced" with the information available here, and a
// single bad report should not abort the whole collection.
func (m *Manager) loadMachineStatus(machine string) (model.Status, bool) {
	data, _, err := m.Store.Get(ossclient.StatusKey(machine))
	if err != nil {
		return model.Status{Machine: machine}, false
	}
	st, err := status.Parse(data)
	if err != nil {
		return model.Status{Machine: machine}, false
	}
	return st, true
}

// extraLocalUsers filters a machine's local profiles down to the ones that
// are neither the bound employee nor the built-in Administrator account.
// These are the accounts an admin did not expect to find on the machine, so
// surfacing them is what lets binding drift or unauthorised use get noticed.
func extraLocalUsers(localUsers []string, boundUser string, bound bool) []string {
	var extra []string
	for _, u := range localUsers {
		if strings.EqualFold(u, administratorAccount) {
			continue
		}
		if bound && strings.EqualFold(u, boundUser) {
			continue
		}
		extra = append(extra, u)
	}
	return extra
}
