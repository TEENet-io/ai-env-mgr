package adminweb

import (
	"sort"
	"strconv"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// distribution is one small panel on the overview: how the fleet splits by
// something -- agent version, Codex version, AppLocker mode, silence.
type distribution struct {
	Title string
	Rows  []countRow
	Total int
}

func itoa(n int) string { return strconv.Itoa(n) }

// fleetDistributions computes the four panels from the same machine list the
// table renders. Rows are sorted by count, ties by label, so the top of each
// panel is what most of the fleet is on.
func fleetDistributions(machines []admincore.MachineState, now time.Time) []distribution {
	agent, codex, applocker, silence := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	for _, m := range machines {
		agent[orLabel(m.Status.AgentVersion, "未知")]++
		codex[orLabel(m.Status.CodexVersion, "未安装")]++
		applocker[orLabel(m.Status.AppLockerMode, "未管理")]++
		silence[silenceBucket(m.Status.LastSync, now)]++
	}
	total := len(machines)
	dist := func(title string, counts map[string]int, order []string) distribution {
		d := distribution{Title: title, Total: total}
		for label, n := range counts {
			d.Rows = append(d.Rows, countRow{Label: label, N: n, Pct: pct(n, total)})
		}
		if order != nil {
			rank := map[string]int{}
			for i, l := range order {
				rank[l] = i
			}
			sort.Slice(d.Rows, func(i, j int) bool { return rank[d.Rows[i].Label] < rank[d.Rows[j].Label] })
		} else {
			sort.Slice(d.Rows, func(i, j int) bool {
				if d.Rows[i].N != d.Rows[j].N {
					return d.Rows[i].N > d.Rows[j].N
				}
				return model.CompareVersions(d.Rows[i].Label, d.Rows[j].Label) > 0
			})
		}
		return d
	}
	return []distribution{
		dist("agent 版本", agent, nil),
		dist("Codex 版本", codex, nil),
		dist("AppLocker", applocker, nil),
		dist("多久没上报", silence, silenceOrder),
	}
}

var silenceOrder = []string{"15 分钟内", "1 小时内", "1 天内", "7 天内", "超过 7 天", "从未"}

func silenceBucket(lastSync string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, lastSync)
	if err != nil || lastSync == "" {
		return "从未"
	}
	age := now.Sub(t)
	switch {
	case age <= 15*time.Minute:
		return "15 分钟内"
	case age <= time.Hour:
		return "1 小时内"
	case age <= 24*time.Hour:
		return "1 天内"
	case age <= 7*24*time.Hour:
		return "7 天内"
	}
	return "超过 7 天"
}

func orLabel(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}

func pct(n, total int) int {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}
