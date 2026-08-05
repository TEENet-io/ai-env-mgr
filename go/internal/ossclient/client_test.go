package ossclient

import "testing"

func TestDataCollectKey(t *testing.T) {
	cases := []struct{ user, rel, want string }{
		{"work1", ".claude/projects/p/a.jsonl", "agent_workdir/work1/data_collect/.claude/projects/p/a.jsonl"},
		{"work1", ".codex/sessions/2026/rollout-x.jsonl", "agent_workdir/work1/data_collect/.codex/sessions/2026/rollout-x.jsonl"},
		{"work1", `.claude\projects\a.jsonl`, "agent_workdir/work1/data_collect/.claude/projects/a.jsonl"},   // 反斜杠归一
		{"work1", "../../etc/passwd", "agent_workdir/work1/data_collect/etc/passwd"},                          // .. 不得逃逸
	}
	for _, c := range cases {
		if got := DataCollectKey(c.user, c.rel); got != c.want {
			t.Errorf("DataCollectKey(%q,%q)=%q want %q", c.user, c.rel, got, c.want)
		}
	}
}
