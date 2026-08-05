//go:build windows

package status

import (
	"os/exec"
	"strings"
)

// AppLockerMode reports the effective AppLocker enforcement mode.
//
// AppLocker is baked into the golden image rather than pushed through OSS, so
// the agent only observes it. Reporting the mode lets the admin spot machines
// still in audit mode, where portable browsers would not be blocked.
func AppLockerMode() string {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", "Get-AppLockerPolicy -Effective -Xml")
	out, err := cmd.Output()
	if err != nil {
		return "Unknown"
	}
	xml := string(out)
	switch {
	case strings.Contains(xml, `EnforcementMode="Enabled"`):
		return "Enforce"
	case strings.Contains(xml, `EnforcementMode="AuditOnly"`):
		return "Audit"
	default:
		return "None"
	}
}
