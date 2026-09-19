package model

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.16", "1.2.9", 1},
		{"1.2.9", "1.2.16", -1},
		{"v1.2.16", "1.2.16", 0},
		{"1.3.0", "1.2.16", 1},
		{"1.2.16", "1.2.16.1", -1},
		{"dev", "1.2.16", -1}, // 非数字视为最低
		{"", "1.2.16", -1},
		{"1.2.16-rc1", "1.2.16", -1}, // 预发布低于正式
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAgentCanTakeTargets(t *testing.T) {
	if !AgentCanTakeTargets("1.2.16") || !AgentCanTakeTargets("1.3.0") {
		t.Fatal("1.2.16 and later must be able to take per-machine targets")
	}
	if AgentCanTakeTargets("1.2.15") || AgentCanTakeTargets("") {
		t.Fatal("older agents only read the fleet policy")
	}
}
