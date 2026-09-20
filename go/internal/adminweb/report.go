package adminweb

import (
	"sort"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// countRow is one labelled count in a report or a distribution.
type countRow struct {
	Label string
	N     int
	Pct   int
}

// versionReport is how one version fared across the machines it was aimed
// at: outcomes per machine (latest attempt), how long success took, and
// what the failures said.
type versionReport struct {
	Product, Version           string
	Succeeded, Failed, Pending int
	Excluded, Cancelled        int
	MedianToSuccess            time.Duration
	HasMedian                  bool
	TopFailures                []countRow
}

// reportByVersion aggregates targets by the artifact they name, counting
// each machine once per version (its latest generation), like a rollout's
// own summary does.
func reportByVersion(targets []repo.Target, artifacts map[string]repo.Artifact) []versionReport {
	type key struct{ product, version string }
	latest := map[key]map[string]repo.Target{} // version -> device -> latest target
	for _, t := range targets {
		a, ok := artifacts[t.ArtifactID]
		if !ok {
			continue
		}
		k := key{a.Product, a.Version}
		if latest[k] == nil {
			latest[k] = map[string]repo.Target{}
		}
		if have, ok := latest[k][t.DeviceID]; !ok || t.Generation > have.Generation {
			latest[k][t.DeviceID] = t
		}
	}
	var out []versionReport
	for k, byDevice := range latest {
		rep := versionReport{Product: k.product, Version: k.version}
		var durations []time.Duration
		failures := map[string]int{}
		for _, t := range byDevice {
			switch t.Status {
			case repo.TargetSucceeded:
				rep.Succeeded++
				if t.FinishedAt != nil {
					durations = append(durations, t.FinishedAt.Sub(t.CreatedAt))
				}
			case repo.TargetFailed:
				rep.Failed++
				failures[t.ResultNote]++
			case repo.TargetPending:
				rep.Pending++
			case repo.TargetExcluded:
				rep.Excluded++
			case repo.TargetCancelled:
				rep.Cancelled++
			}
		}
		if len(durations) > 0 {
			rep.MedianToSuccess, rep.HasMedian = median(durations), true
		}
		for note, n := range failures {
			rep.TopFailures = append(rep.TopFailures, countRow{Label: note, N: n})
		}
		sort.Slice(rep.TopFailures, func(i, j int) bool {
			if rep.TopFailures[i].N != rep.TopFailures[j].N {
				return rep.TopFailures[i].N > rep.TopFailures[j].N
			}
			return rep.TopFailures[i].Label < rep.TopFailures[j].Label
		})
		if len(rep.TopFailures) > 3 {
			rep.TopFailures = rep.TopFailures[:3]
		}
		out = append(out, rep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Product != out[j].Product {
			return out[i].Product < out[j].Product
		}
		return model.CompareVersions(out[i].Version, out[j].Version) > 0
	})
	return out
}

// deferredRow is a machine that has been putting an update off for longer
// than a working day: the one that is always in use, or always out of disk.
type deferredRow struct {
	Hostname string
	Product  string
	Version  string
	Reason   string
	Since    time.Time
}

// alwaysDeferred finds pending targets older than a day whose machine's last
// report says it deferred the install.
func alwaysDeferred(targets []repo.Target, artifacts map[string]repo.Artifact, hostnames map[string]string, reports map[string]*model.Status, now time.Time) []deferredRow {
	var out []deferredRow
	for _, t := range targets {
		if t.Status != repo.TargetPending || now.Sub(t.CreatedAt) < 24*time.Hour {
			continue
		}
		status := reports[t.DeviceID]
		if status == nil {
			continue
		}
		a, ok := artifacts[t.ArtifactID]
		if !ok {
			continue
		}
		_, seenTarget, seenGen, state, reason := reportFor(t.Product, status)
		if seenTarget != a.Version || seenGen != t.Generation || state != "deferred" {
			continue
		}
		label := reason
		switch reason {
		case "in_use":
			label = "正在使用"
		case "disk":
			label = "磁盘不足"
		}
		host := hostnames[t.DeviceID]
		if host == "" {
			host = t.DeviceID
		}
		out = append(out, deferredRow{Hostname: host, Product: t.Product, Version: a.Version, Reason: label, Since: t.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// humanDuration is a duration a person reads: minutes below two hours,
// hours below two days, days after.
func humanDuration(d time.Duration) string {
	switch {
	case d < 2*time.Hour:
		return itoa(int(d.Minutes())) + " 分钟"
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + " 小时"
	default:
		return itoa(int(d.Hours()/24)) + " 天"
	}
}

// median is the middle duration, or the mean of the two middle ones when
// there is no single middle: two machines at 10 and 30 minutes report 20,
// not 30.
func median(d []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
