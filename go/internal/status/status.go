// Package status assembles the agent's self-report and helps the admin read it.
//
// Each agent writes one report per sync to {user}/status.json; the admin reads
// them all to tell which machines are healthy, stale or silent.
package status

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// unreachableAge is returned for reports whose timestamp cannot be parsed.
// It is deliberately enormous so such machines always sort as stale rather
// than silently looking fresh.
const unreachableAge = 100 * 365 * 24 * time.Hour

// Report carries everything one sync cycle learned, ready to be turned into
// a Status. It is a struct rather than a long parameter list so that adding a
// field later does not silently reorder existing call sites.
type Report struct {
	Machine         string
	BoundUser       string
	LocalUsers      []string
	BoundUserExists bool
	Version         string
	Policy          model.Policy
	PolicyETag      string
	CredsETag       string
	Interval        int
	CredsApplied    bool
	CollectEnabled  bool
	CollectUploaded int
	CodexVersion    string
	CodexState      string
	Warnings        []string
	Errors          []string
}

// Build fills in a Status from the results of one sync cycle.
func Build(r Report) model.Status {
	errs := r.Errors
	if errs == nil {
		// A nil slice marshals to null; the admin expects an array.
		errs = []string{}
	}
	users := r.LocalUsers
	if users == nil {
		users = []string{}
	}
	return model.Status{
		Machine:             r.Machine,
		BoundUser:           r.BoundUser,
		LocalUsers:          users,
		BoundUserExists:     r.BoundUserExists,
		LastSync:            time.Now().UTC().Format(time.RFC3339),
		AgentVersion:        r.Version,
		PolicyETag:          r.PolicyETag,
		CredsETag:           r.CredsETag,
		BlockEnabled:        r.Policy.BlockEnabled,
		BlockedDomains:      len(r.Policy.BlockedDomains),
		SyncIntervalMinutes: r.Interval,
		CredsApplied:        r.CredsApplied,
		AppLockerMode:       AppLockerMode(),
		CollectEnabled:      r.CollectEnabled,
		CollectUploaded:     r.CollectUploaded,
		CodexVersion:        r.CodexVersion,
		CodexState:          r.CodexState,
		Warnings:            r.Warnings,
		Errors:              errs,
	}
}

// Marshal renders a status document for upload.
func Marshal(s model.Status) ([]byte, error) {
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode status: %w", err)
	}
	return out, nil
}

// Parse reads a status document.
func Parse(data []byte) (model.Status, error) {
	var s model.Status
	if err := json.Unmarshal(data, &s); err != nil {
		return model.Status{}, fmt.Errorf("parse status: %w", err)
	}
	return s, nil
}

// Age reports how long ago a report was written. An unparseable or future
// timestamp yields a very large duration so the caller treats the machine as
// stale instead of trusting a bad clock.
func Age(s model.Status) time.Duration {
	t, err := time.Parse(time.RFC3339, s.LastSync)
	if err != nil {
		return unreachableAge
	}
	d := time.Since(t)
	if d < 0 {
		// A report from the future means clock skew; do not report it as fresh.
		return unreachableAge
	}
	return d
}
