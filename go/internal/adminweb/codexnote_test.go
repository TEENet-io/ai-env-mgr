package adminweb

import (
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// What the machines page says beside a Codex version.
//
// The question after publishing is "did it land", and a bare version string
// does not answer it. These are the cases that look alike on the page and mean
// different things: an update still moving, one that keeps stepping aside, and
// one that failed and will not retry.
func TestCodexNote(t *testing.T) {
	const target = "26.810.52044-b1"

	for _, tc := range []struct {
		name      string
		installed string
		state     string
		target    string
		want      string
		wantSev   string
	}{
		{"on the published version", target, "", target, "", ""},
		{"still on the old one", "26.803.81509-b1", "", target, "待更新", "warn"},
		{"nothing installed yet", "", "", target, "待更新", "warn"},
		{"nothing published", "", "", "", "", ""},
		// A machine that never took an update looks identical to one on the
		// target unless the published version is known, which is why the page
		// loads the policy.
		{"nothing published, something installed", "26.803.81509-b1", "", "", "", ""},
		{"downloading", "", agentcore.CodexDownloading, target, "下载中", "warn"},
		{"installing", "", agentcore.CodexInstalling, target, "安装中", "warn"},
		{"deferred: Codex was open or the disk was full", "26.803.81509-b1", agentcore.CodexDeferred, target, "已推迟", "warn"},
		// The only one that will not resolve itself: the agent tries a failed
		// version once and then stops.
		{"failed", "26.803.81509-b1", agentcore.CodexFailed, target, "安装失败", "err"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Reporting normally: the tag is about an update, so the
			// machine has to be one that could take it.
			m := admincore.MachineState{
				Machine: "pc1",
				Bound:   true,
				Status: model.Status{
					LastSync:     hoursAgo(0),
					CodexVersion: tc.installed,
					CodexState:   tc.state,
				},
			}
			got := codexNote(m, tc.target)
			if got != tc.want {
				t.Fatalf("note = %q, want %q", got, tc.want)
			}
			if sev := codexNoteSeverity(got); sev != tc.wantSev {
				t.Fatalf("severity = %q, want %q", sev, tc.wantSev)
			}
		})
	}
}

// An agent older than 1.2.5 has no Codex support at all, so it will never
// install and never report a version. It has to read as "待更新" rather than as
// anything reassuring -- the reason it is stuck is the agent, and the agent
// version is right beside it in the same row.
func TestCodexNoteOnAnAgentTooOldToInstall(t *testing.T) {
	m := admincore.MachineState{
		Machine: "pc1",
		Bound:   true,
		Status:  model.Status{LastSync: hoursAgo(0), AgentVersion: "1.2.4"},
	}
	if got := codexNote(m, "26.810.52044-b1"); got != "待更新" {
		t.Fatalf("note = %q, want 待更新", got)
	}
}

// A machine that is asleep, shut down or has never reported is behind on every
// published version by definition, and the state column already says why.
// Tagging those too put 待更新 on nearly every row and buried the one machine
// whose install had actually failed.
func TestCodexNoteStaysQuietForUnreachableMachines(t *testing.T) {
	const target = "26.810.52044-b1"
	quiet := []admincore.MachineState{
		{Machine: "asleep", Bound: true,
			Status: model.Status{LastSync: hoursAgo(3), LastEvent: "suspend"}},
		{Machine: "stopped", Bound: true,
			Status: model.Status{LastSync: hoursAgo(9), LastEvent: "stopped"}},
		{Machine: "never", Bound: true, Missing: true},
		{Machine: "stale", Bound: true, Stale: true, Status: model.Status{LastSync: hoursAgo(120)}},
	}
	for _, m := range quiet {
		if got := codexNote(m, target); got != "" {
			t.Errorf("%s (%s) was tagged %q", m.Machine, m.Health(), got)
		}
	}

	// But a machine that is up and behind still has to be visible.
	live := admincore.MachineState{Machine: "pc1", Bound: true,
		Status: model.Status{LastSync: hoursAgo(0), CodexVersion: "26.803.81509-b1"}}
	if got := codexNote(live, target); got != "待更新" {
		t.Fatalf("a reporting machine behind the target was tagged %q", got)
	}

	// An agent's own report is shown whatever the machine's state: a failure
	// recorded before it went to sleep is still the thing to look at.
	asleepButFailed := admincore.MachineState{Machine: "pc2", Bound: true,
		Status: model.Status{LastSync: hoursAgo(3), LastEvent: "suspend",
			CodexState: agentcore.CodexFailed}}
	if got := codexNote(asleepButFailed, target); got != "安装失败" {
		t.Fatalf("a recorded failure was hidden by the machine's state: %q", got)
	}
}

func hoursAgo(h int) string {
	return time.Now().Add(-time.Duration(h) * time.Hour).UTC().Format(time.RFC3339)
}
