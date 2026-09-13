package creds

import (
	"reflect"
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
	if got := stopCodexArgs("work1"); !reflect.DeepEqual(got, want) {
		t.Errorf("stopCodexArgs(%q) =\n%q\nwant\n%q", "work1", got, want)
	}
}

// A machine-wide kill would take every other session's work with it, so the
// filter must be present on every command, not merely on the first.
func TestStopCodexArgsNeverKillMachineWide(t *testing.T) {
	for _, args := range stopCodexArgs("work1") {
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
		args := stopCodexArgs(user)
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
