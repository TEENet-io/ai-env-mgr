package creds

import (
	"fmt"
	"regexp"
	"strings"
)

// aiToolProcesses are the executables that hold AI tool credentials open in
// memory. Codex ships as the Electron app ChatGPT.exe (launched by
// Codex.vbs); codex.exe and claude.exe are the CLIs beside it.
//
// Why kill them at all: these tools read their token once at startup and do
// not watch auth.json (or the Claude equivalents) for changes -- Codex's own
// source is explicit that an external edit is not picked up until the process
// reloads. Overwriting the files on disk while the tool keeps running
// therefore changes nothing until it restarts, and a token that has since
// been revoked goes on working.
var aiToolProcesses = []string{"ChatGPT.exe", "codex.exe", "claude.exe"}

// localAccountPattern is what a Windows local account name may look like
// before it is pasted into a taskkill filter.
//
// The filter string is built by concatenation, and the binding it comes from
// is written by the console -- so this is the boundary where a name that is
// not a name has to be refused. A `*` would turn "end this employee's Codex"
// into "end everybody's on this machine", which is exactly the machine-wide
// kill the filter exists to prevent; a name with a space changes how taskkill
// parses the expression. Windows itself forbids all of "/\[]:;|=,+*?<>%"
// and spaces at the ends, so nothing legitimate is lost by being stricter
// than Windows and allowing only these.
var localAccountPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// stopCodexArgs builds the taskkill argument lists that end one user's AI
// tools, one list per executable.
//
// The /FI "USERNAME eq <user>" filter is the whole point. These are
// multi-session cloud desktops: a bare `taskkill /F /IM ChatGPT.exe` reaches
// every session on the box, so rotating one employee's token would take the
// work of everyone else signed in at that moment with it. The filter confines
// the kill to the account the console actually acted on.
//
// The argument text here is exactly what reaches taskkill. The quotes seen on
// a command line belong to the shell; exec passes `USERNAME eq work1` as one
// argument without them. A DOMAIN\ prefix is stripped because the filter
// matches the bare account name that owns the session.
func stopCodexArgs(user string) ([][]string, error) {
	name := localAccountName(user)
	if !localAccountPattern.MatchString(name) {
		return nil, fmt.Errorf("refusing to kill for invalid user name %q", user)
	}
	args := make([][]string, 0, len(aiToolProcesses))
	for _, proc := range aiToolProcesses {
		args = append(args, []string{"/F", "/FI", "USERNAME eq " + name, "/IM", proc})
	}
	return args, nil
}

// localAccountName reduces `CORP\work1` or `.\work1` to `work1`.
func localAccountName(user string) string {
	user = strings.TrimSpace(user)
	if i := strings.LastIndexAny(user, `\/`); i >= 0 {
		return user[i+1:]
	}
	return user
}

// imageName pulls the executable out of a taskkill argument list for an error
// message: the value after /IM, rather than whichever argument happens to be
// last. A later flag appended to the list would otherwise quietly turn every
// failure report into a lie.
func imageName(args []string) string {
	for i, a := range args {
		if strings.EqualFold(a, "/IM") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return "taskkill"
}

// taskkillKilled counts the processes taskkill reports having ended: one
// success line each.
//
// It is the only count available -- taskkill has no machine-readable output
// and one session can hold several codex.exe at once -- and it is what the
// console shows the administrator who pressed the button. These desktops run
// a Chinese Windows, so matching only the English word would have reported
// "no process" for a kill that actually took three, which is worse than
// saying nothing.
func taskkillKilled(output string) int {
	n := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(strings.ToUpper(line), "SUCCESS") || strings.Contains(line, "成功") {
			n++
		}
	}
	return n
}

// taskkillNoMatch reports whether taskkill's output means "nothing matched"
// rather than a real failure.
//
// Nothing matching is the ordinary case, not an error: an employee who does
// not happen to have Codex open must not make a credential rotation look
// broken. taskkill also signals it with exit code 128, which is what
// StopAIToolsFor relies on first; the text is checked as well because these
// machines run a Chinese Windows whose wording differs from the English one
// and neither is a documented contract.
func taskkillNoMatch(output string) bool {
	lower := strings.ToLower(output)
	markers := []string{
		"not found", // ERROR: The process "x" not found.
		"no tasks",  // INFO: No tasks running with the specified criteria.
		"没有找到进程",    // zh-CN: 错误: 没有找到进程 "x"。
		"没有运行的任务",   // zh-CN: 信息: 没有运行的任务匹配指定标准。
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
