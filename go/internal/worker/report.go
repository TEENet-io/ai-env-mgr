package worker

import (
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// ReportFromStatus maps the agent's object onto the columns the console
// filters on. The whole object rides along as well.
func ReportFromStatus(deviceID string, s model.Status, raw []byte, etag string) repo.DeviceReport {
	r := repo.DeviceReport{
		DeviceID:          deviceID,
		AgentVersion:      s.AgentVersion,
		BoundWindowsUser:  s.BoundUser,
		BoundUserExists:   &s.BoundUserExists,
		PolicyETag:        s.PolicyETag,
		CredsETag:         s.CredsETag,
		CredsApplied:      &s.CredsApplied,
		AppLockerMode:     s.AppLockerMode,
		CollectEnabled:    &s.CollectEnabled,
		CollectUploaded:   s.CollectUploaded,
		CodexVersion:      s.CodexVersion,
		CodexState:        s.CodexState,
		CodexRestartNonce: s.CodexRestartNonce,
		CodexRestartNote:  s.CodexRestartNote,
		LastEvent:         s.LastEvent,
		ErrorCount:        len(s.Errors),
		WarningCount:      len(s.Warnings),
		Report:            raw,
		SourceETag:        etag,
	}
	r.LastSyncAt = parseStamp(s.LastSync)
	r.CodexRestartAt = parseStamp(s.CodexRestartAt)
	r.LastEventAt = parseStamp(s.LastEventAt)
	return r
}

func parseStamp(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
