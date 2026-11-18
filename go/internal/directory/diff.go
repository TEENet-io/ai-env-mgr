package directory

import "sort"

// What happened to one person between two readings of the list.
const (
	ChangeAdded     = "added"     // on the list for the first time
	ChangeRenamed   = "renamed"   // display name or username
	ChangeEmail     = "email"     // email address
	ChangeRole      = "role"      // ops / admin / employee, or an admin's ticket role
	ChangeDisabled  = "disabled"  // was active, now disabled or deleted
	ChangeReenabled = "reenabled" // was disabled or deleted, now active
	ChangeDeleted   = "deleted"   // was disabled, now deleted: nothing more to take away
)

// MaxDisablesPerSync is how many people one reading may take access away
// from before the console stops and asks. Three is a normal week of
// leavers; more at once is far likelier a fault in the tool, or somebody
// with the tool's admin rights doing damage, than a real event.
const MaxDisablesPerSync = 3

// Change is one difference. Before is the zero Account for ChangeAdded.
type Change struct {
	Kind          string
	Before, After Account
}

// Plan is everything one reading changes, after the safety rules.
type Plan struct {
	// Changes are applied in this order: additions and edits first, then
	// what is taken away, so a person renamed and disabled in one reading
	// is recorded under the new name.
	Changes []Change
	// Held are the disables that were not applied because there were too
	// many at once. The local copy keeps the earlier status for them, so
	// the next reading finds them again until somebody confirms.
	Held []Change
	// Missing are people the console knows who were absent from this
	// reply. Nothing is done to them.
	Missing []Account
}

// Compare works out the plan from the local copy and a validated reply.
func Compare(known map[string]Account, list List) Plan {
	var p Plan
	var disables []Change
	present := make(map[string]bool, len(list.Accounts))
	for _, now := range list.Accounts {
		present[now.ID] = true
		before, ok := known[now.ID]
		if !ok {
			p.Changes = append(p.Changes, Change{Kind: ChangeAdded, After: now})
			continue
		}
		if before.Name != now.Name || before.Username != now.Username {
			p.Changes = append(p.Changes, Change{Kind: ChangeRenamed, Before: before, After: now})
		}
		if before.Email != now.Email {
			p.Changes = append(p.Changes, Change{Kind: ChangeEmail, Before: before, After: now})
		}
		// A deleted account's record carries no ticket role; that is not a change.
		ticketChanged := before.TicketRole != now.TicketRole && now.Status != StatusDeleted
		if before.Role != now.Role || ticketChanged {
			p.Changes = append(p.Changes, Change{Kind: ChangeRole, Before: before, After: now})
		}
		switch {
		case before.Active() && !now.Active():
			disables = append(disables, Change{Kind: ChangeDisabled, Before: before, After: now})
		case !before.Active() && now.Active():
			p.Changes = append(p.Changes, Change{Kind: ChangeReenabled, Before: before, After: now})
		case before.Status == StatusDisabled && now.Status == StatusDeleted:
			p.Changes = append(p.Changes, Change{Kind: ChangeDeleted, Before: before, After: now})
		}
	}
	if len(disables) > MaxDisablesPerSync {
		p.Held = disables
	} else {
		p.Changes = append(p.Changes, disables...)
	}
	for id, a := range known {
		if !present[id] {
			p.Missing = append(p.Missing, a)
		}
	}
	sort.Slice(p.Missing, func(i, j int) bool { return p.Missing[i].ID < p.Missing[j].ID })
	return p
}

// Store is what the console keeps for the next reading: the reply, except
// that a held disable keeps the status it had. Missing people are not part
// of it; the caller leaves their rows alone.
func (p Plan) Store(list List) []Account {
	held := make(map[string]Account, len(p.Held))
	for _, c := range p.Held {
		held[c.After.ID] = c.Before
	}
	out := make([]Account, 0, len(list.Accounts))
	for _, a := range list.Accounts {
		if before, ok := held[a.ID]; ok {
			a.Status = before.Status
		}
		out = append(out, a)
	}
	return out
}
