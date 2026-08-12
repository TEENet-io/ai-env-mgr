package main

import (
	"os"

	"golang.org/x/term"
)

// useColor is decided once at startup: colourise only when writing to a real
// terminal and the operator has not opted out (NO_COLOR). Piped or redirected
// output stays plain, so parsing tools see clean text.
var useColor = false

func init() {
	if os.Getenv("NO_COLOR") != "" {
		return
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	useColor = enableVT() // platform hook: enables VT on Windows, true elsewhere
}

// ANSI SGR codes.
const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cGrey   = "\x1b[90m"
)

// paint colours s for direct (non-tabwriter) output. A no-op when colour is
// off.
func paint(s, code string) string {
	if !useColor {
		return s
	}
	return code + s + cReset
}

// cell is paint for text written INTO a tabwriter. The ANSI codes are bracketed
// by tabwriter's Escape byte (0xff) so their width is not counted -- otherwise a
// coloured cell would throw off column alignment. The visible text stays outside
// the brackets so it still counts.
func cell(s, code string) string {
	if !useColor {
		return s
	}
	return "\xff" + code + "\xff" + s + "\xff" + cReset + "\xff"
}

// stateColor tints a STATE cell by how much it needs attention: green healthy,
// grey expected-away, yellow assign-me, red investigate. For tabwriter output.
func stateColor(state string) string {
	switch state {
	case "OK":
		return cell(state, cGreen)
	case "SLEEPING", "STOPPED", "OFFLINE":
		return cell(state, cGrey)
	case "UNBOUND":
		return cell(state, cYellow)
	default: // STALE, NO REPORT, DISABLED USER, USER MISSING, N ERROR(S)
		return cell(state, cRed)
	}
}
