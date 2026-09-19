package model

import (
	"strconv"
	"strings"
)

// MinTargetAgentVersion is the first agent that reads per-machine release
// targets from its binding object. Anything older only reads the fleet
// policy, so a target aimed at it would sit there unread and the console
// refuses to create one.
const MinTargetAgentVersion = "1.2.16"

// AgentCanTakeTargets reports whether an agent of this version reads
// per-machine targets.
func AgentCanTakeTargets(version string) bool {
	return CompareVersions(version, MinTargetAgentVersion) >= 0
}

// CompareVersions orders two dotted version strings: -1, 0 or 1.
//
// A leading "v" is ignored. Numeric components compare as numbers, so 1.2.16
// is after 1.2.9. A pre-release suffix ("1.2.16-rc1") sorts before the plain
// version. A string with no numeric component at all ("dev", "") is the
// lowest of all: an agent that cannot say what it is gets nothing aimed at it.
func CompareVersions(a, b string) int {
	an, apre := splitVersion(a)
	bn, bpre := splitVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		x, y := 0, 0
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "": // the plain version is after its pre-releases
		return 1
	case bpre == "":
		return -1
	case apre < bpre:
		return -1
	default:
		return 1
	}
}

// splitVersion returns the numeric components and the pre-release suffix.
// A version with no numeric component returns nil, which compares below
// everything.
func splitVersion(v string) ([]int, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	var nums []int
	for _, part := range strings.Split(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, ""
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return nil, ""
	}
	return nums, pre
}
