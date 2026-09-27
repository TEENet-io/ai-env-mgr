package adminweb

import "testing"

func TestTaskStatusMatches(t *testing.T) {
	tests := []struct {
		status, filter string
		want           bool
	}{
		{"pending", "open", true},
		{"running", "open", true},
		{"retry_wait", "open", true},
		{"succeeded", "open", false},
		{"failed", "failed", true},
		{"blocked", "failed", true},
		{"cancelled", "done", true},
		{"superseded", "done", true},
		{"running", "done", false},
		{"unknown", "", true},
	}
	for _, tt := range tests {
		if got := taskStatusMatches(tt.status, tt.filter); got != tt.want {
			t.Errorf("taskStatusMatches(%q, %q) = %v, want %v", tt.status, tt.filter, got, tt.want)
		}
	}
}
