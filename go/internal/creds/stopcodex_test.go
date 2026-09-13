package creds

import (
	"reflect"
	"strings"
	"testing"
)

// The exact argument text is the test's whole point. taskkill's /FI filter
// cannot be exercised off Windows, so the one thing that can go wrong here --
// a stray quote, a missing /F, a filter that reads "USERNAME=work1" and
// therefore matches nothing while still exiting 0 -- has to be caught by
// reading the arguments themselves.
func TestStopCodexArgsAreScopedToTheUser(t *testing.T) {
	want := [][]string{
		{"/F", "/FI", "USERNAME eq work1", "/IM", "ChatGPT.exe"},
		{"/F", "/FI", "USERNAME eq work1", "/IM", "codex.exe"},
		{"/F", "/FI", "USERNAME eq work1", "/IM", "claude.exe"},
	}
	got, err := stopCodexArgs("work1")
	if err != nil {
		t.Fatalf("stopCodexArgs(%q): %v", "work1", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stopCodexArgs(%q) =\n%q\nwant\n%q", "work1", got, want)
	}
}

// A machine-wide kill would take every other session's work with it, so the
// filter must be present on every command, not merely on the first.
func TestStopCodexArgsNeverKillMachineWide(t *testing.T) {
	all, err := stopCodexArgs("work1")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range all {
		found := false
		for _, a := range args {
			if a == "/FI" {
				found = true
			}
		}
		if !found {
			t.Errorf("%q has no /FI filter: it would kill the whole machine", args)
		}
	}
}

func TestStopCodexArgsStripsDomainPrefix(t *testing.T) {
	for _, user := range []string{`CORP\work1`, `.\work1`, "  work1  "} {
		args, err := stopCodexArgs(user)
		if err != nil {
			t.Fatalf("stopCodexArgs(%q): %v", user, err)
		}
		if args[0][2] != "USERNAME eq work1" {
			t.Errorf("stopCodexArgs(%q) filter = %q, want %q", user, args[0][2], "USERNAME eq work1")
		}
	}
}

// "Nothing matched" must read as success: an employee who did not have Codex
// open is the common case, and reporting it as a failure would put a red
// error on routine token rotations.
func TestTaskkillNoMatch(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"INFO: No tasks running with the specified criteria.", true},
		{`ERROR: The process "codex.exe" not found.`, true},
		{"信息: 没有运行的任务匹配指定标准。", true},
		{`错误: 没有找到进程 "codex.exe"。`, true},
		{`SUCCESS: The process "ChatGPT.exe" with PID 4242 has been terminated.`, false},
		{"ERROR: Access is denied.", false},
		{"", false},
	}
	for _, c := range cases {
		if got := taskkillNoMatch(c.out); got != c.want {
			t.Errorf("taskkillNoMatch(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}

// The count is what the console reports back, and these machines run a
// Chinese Windows: matching only the English word would report "no process"
// for a kill that actually ended three.
func TestTaskkillKilledCountsBothLocales(t *testing.T) {
	cases := []struct {
		out  string
		want int
	}{
		{"", 0},
		{"INFO: No tasks running with the specified criteria.", 0},
		{`SUCCESS: The process "ChatGPT.exe" with PID 4242 has been terminated.`, 1},
		{`SUCCESS: The process "codex.exe" with PID 1 has been terminated.` + "\n" +
			`SUCCESS: The process "codex.exe" with PID 2 has been terminated.`, 2},
		{"成功: 给进程发送了终止信号，进程的 PID 为 4242。", 1},
		{"成功: 已终止 PID 1 的进程。\r\n成功: 已终止 PID 2 的进程。\r\n", 2},
		{"错误: 没有找到进程 \"codex.exe\"。", 0},
	}
	for _, c := range cases {
		if got := taskkillKilled(c.out); got != c.want {
			t.Errorf("taskkillKilled(%q) = %d, want %d", c.out, got, c.want)
		}
	}
}

// The filter is built by pasting the name into a string, and the name comes
// from a binding the console writes. This is the boundary where something
// that is not a name has to be turned away -- above all `*`, which would
// turn "end this employee's Codex" into the machine-wide kill the filter
// exists to prevent.
func TestStopCodexArgsRejectsNamesThatAreNotNames(t *testing.T) {
	bad := []string{
		"*",          // matches every account on the box
		"work*",      // same, less obviously
		`corp\`,      // a domain prefix and nothing after it
		"",           // no user at all
		"   ",        // nor after trimming
		"work 1",     // a space changes how taskkill parses the filter
		`work"1`,     // quote
		"work1;calc", // command-ish
		"work$1",     // not a character Windows allows in an account name
		strings.Repeat("a", 65),
	}
	for _, user := range bad {
		args, err := stopCodexArgs(user)
		if err == nil {
			t.Errorf("stopCodexArgs(%q) was accepted and produced %q", user, args)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to kill for invalid user name") {
			t.Errorf("stopCodexArgs(%q) error = %v, want it to say why", user, err)
		}
		if args != nil {
			t.Errorf("stopCodexArgs(%q) returned commands alongside an error: %q", user, args)
		}
	}
}

// The names real accounts actually have must keep working; a check that
// refuses those is a check that gets deleted.
func TestStopCodexArgsAcceptsOrdinaryNames(t *testing.T) {
	for _, user := range []string{"work1", "Administrator", "li.si", "zhang-san", "a_b", "A", strings.Repeat("a", 64)} {
		if _, err := stopCodexArgs(user); err != nil {
			t.Errorf("stopCodexArgs(%q) refused a legitimate name: %v", user, err)
		}
	}
}

// The error text names the executable that failed. Reading it off the end of
// the argument list happened to work; one more flag appended to the command
// and every failure report would have named the flag instead.
func TestImageNameComesFromTheIMFlag(t *testing.T) {
	if got := imageName([]string{"/F", "/FI", "USERNAME eq work1", "/IM", "codex.exe"}); got != "codex.exe" {
		t.Errorf("imageName = %q, want codex.exe", got)
	}
	if got := imageName([]string{"/F", "/IM", "ChatGPT.exe", "/T"}); got != "ChatGPT.exe" {
		t.Errorf("imageName = %q, want ChatGPT.exe (not the trailing flag)", got)
	}
	if got := imageName([]string{"/F", "/IM"}); got != "taskkill" {
		t.Errorf("imageName = %q, want the fallback when /IM has no value", got)
	}
}
