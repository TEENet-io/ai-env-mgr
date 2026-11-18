package dbstore

import (
	"sort"
	"testing"
)

func TestCompareVersionsIsNumeric(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.5.19", "1.5.9", 1},
		{"1.5.20", "1.5.19", 1},
		{"1.5.9", "1.5.9", 0},
		{"1.5", "1.5.1", -1},
		{"1.10.0", "1.9.9", 1},
		{"26.901.51231-b44", "26.901.51231-b13", 1},
		{"26.901.51231-b13", "26.901.51231-b8", 1},
		{"26.901.51231-b8", "26.810.52044-b1", 1},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := compareVersions(tc.b, tc.a); got != -tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestRebuiltLibraryListsNewestVersionFirst(t *testing.T) {
	// The order a rebuilt library registers in: as text.
	versions := []string{"1.5.0", "1.5.1", "1.5.10", "1.5.19", "1.5.2", "1.5.9", "1.5.20"}
	sort.SliceStable(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
	want := []string{"1.5.20", "1.5.19", "1.5.10", "1.5.9", "1.5.2", "1.5.1", "1.5.0"}
	for i := range want {
		if versions[i] != want[i] {
			t.Fatalf("order = %v, want %v", versions, want)
		}
	}
}
