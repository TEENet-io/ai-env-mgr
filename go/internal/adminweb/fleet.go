package adminweb

import "github.com/TEENet-io/ai-env-mgr/internal/admincore"

// fleetSummary is the aggregate the strip at the top of the machine pages
// draws: how much of the fleet is fine, how much wants a look, how much is
// broken. It is the first thing an operator checks, so it is computed from the
// same machine list the table below it renders -- never a second source.
type fleetSummary struct {
	Total, OK, Warn, Bad int
	// Percentages, so the template can size the segments without arithmetic.
	PctOK, PctWarn, PctBad int
}

// classifyMachine sorts one machine into the three-state vocabulary the whole
// console uses.
//
// Bad means somebody has to act: a machine that has never reported may not have
// an agent at all, and one bound to a departed employee is still holding their
// credentials. Warn is everything that is merely not right yet -- unassigned,
// out of contact, or missing the profile its credentials are aimed at.
func classifyMachine(m admincore.MachineState) string {
	switch {
	case m.Missing || m.Disabled:
		return "bad"
	case m.Stale || m.Unbound || m.UserMissing:
		return "warn"
	default:
		return "ok"
	}
}

func summariseFleet(machines []admincore.MachineState) *fleetSummary {
	if len(machines) == 0 {
		return nil
	}
	f := &fleetSummary{Total: len(machines)}
	for _, m := range machines {
		switch classifyMachine(m) {
		case "bad":
			f.Bad++
		case "warn":
			f.Warn++
		default:
			f.OK++
		}
	}
	// Round the first two and give the remainder to the last, so the segments
	// always add up to exactly 100 and the bar never leaves a sliver of ground
	// showing.
	f.PctOK = f.OK * 100 / f.Total
	f.PctWarn = f.Warn * 100 / f.Total
	f.PctBad = 100 - f.PctOK - f.PctWarn
	return f
}
