package main

import (
	"strings"
	"testing"
)

func withColor(t *testing.T, on bool) {
	t.Helper()
	old := useColor
	useColor = on
	t.Cleanup(func() { useColor = old })
}

func TestPaintAndCellNoOpWhenColorOff(t *testing.T) {
	withColor(t, false)
	if paint("x", cGreen) != "x" {
		t.Error("paint should be a no-op when colour is off")
	}
	if cell("x", cGreen) != "x" {
		t.Error("cell should be a no-op when colour is off")
	}
	if stateColor("OK") != "OK" {
		t.Error("stateColor should be a no-op when colour is off")
	}
}

func TestCellBracketsAnsiButPaintDoesNot(t *testing.T) {
	withColor(t, true)
	// The visible text must sit OUTSIDE the 0xff-bracketed escapes so tabwriter
	// counts only its width, keeping columns aligned.
	want := "\xff" + cGreen + "\xff" + "OK" + "\xff" + cReset + "\xff"
	if got := cell("OK", cGreen); got != want {
		t.Fatalf("cell = %q, want %q", got, want)
	}
	// paint is for direct output and must never emit the tabwriter escape byte.
	if strings.ContainsRune(paint("OK", cGreen), '\xff') {
		t.Fatal("paint must not emit the tabwriter escape byte")
	}
}

func TestStateColorSeverity(t *testing.T) {
	withColor(t, true)
	cases := map[string]string{
		"OK": cGreen, "OFFLINE": cGrey, "SLEEPING": cGrey, "STOPPED": cGrey,
		"UNBOUND": cYellow, "STALE": cRed, "NO REPORT": cRed, "USER MISSING": cRed,
	}
	for state, code := range cases {
		if !strings.Contains(stateColor(state), code) {
			t.Errorf("stateColor(%q) should use %q", state, code)
		}
	}
}
