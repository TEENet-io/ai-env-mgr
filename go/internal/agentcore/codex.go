package agentcore

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// CodexInstaller is the local, Windows-specific half of a Codex update. It is
// injected for the same reason Applier and Machine are: the decision logic
// below has to be testable on a machine that has no Codex and no Windows.
type CodexInstaller interface {
	// InstalledVersion reports the version present on this machine, or ""
	// when Codex is not installed.
	InstalledVersion() (string, error)
	// Running reports whether Codex is in use. Installing over a running copy
	// fails on locked files, and would yank the application out from under
	// whoever is working in it.
	Running() (bool, error)
	// Install runs the downloaded installer silently and returns once it is
	// done.
	Install(setupPath, version string) error
	// FreeBytes is the space available where Codex installs.
	FreeBytes() (uint64, error)
}

// Codex update states, reported in status so the administrator can see why a
// machine has not taken an update.
const (
	CodexIdle        = ""
	CodexDeferred    = "deferred" // eligible, but not now (in use, or no room)
	CodexDownloading = "downloading"
	CodexInstalling  = "installing"
	CodexFailed      = "failed"
)

// codexMarkerFile records the last version this machine attempted, so a
// package that fails to install is not retried every sync forever. Clearing
// the target, or publishing a different version, makes it try again.
const codexMarkerFile = "codex-target"

// installHeadroom is what must be free before downloading: the installer
// itself plus room for what it unpacks. A cloud desktop that fills its disk
// mid-install is a worse outcome than an update that waits.
const installHeadroom = 3 << 30 // 3 GiB

// codexEligible reports whether this machine should take the published Codex
// version, and why not when it should not.
//
// The comparison is for EQUALITY, never "newer". Publishing an older version
// is how a bad build is rolled back, and an upgrade-only rule cannot express
// that -- which is precisely when it would be needed.
func codexEligible(pol model.Policy, installed string) (bool, string) {
	if pol.CodexVersion == "" {
		return false, "" // nothing published; also the kill switch
	}
	if pol.CodexKey == "" || pol.CodexSHA256 == "" {
		return false, "codex: published version has no object key or checksum"
	}
	if installed == pol.CodexVersion {
		return false, ""
	}
	// A published version reaches every machine. The staged rollout this used
	// to consult (CodexRolloutPct, hashed against the machine name) is gone:
	// what it was meant to buy -- confidence that the repackaged build is sound
	// -- has to come from accepting it on a real Windows machine, and on a fleet
	// this size a small ring mostly selected nobody. The policy field is still
	// written, for agents old enough to gate on it.
	return true, ""
}

// updateCodex runs one Codex update cycle. It returns the version now
// installed and the state to report, appending any problems to errs.
//
// Nothing here is fatal to the sync: a machine that cannot update Codex still
// has to report its status, apply policy, and stay manageable.
func (s *Syncer) updateCodex(pol model.Policy, errs *[]string) (installed, state string) {
	if s.Codex == nil {
		return "", CodexIdle
	}
	installed, err := s.Codex.InstalledVersion()
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("codex: read installed version: %v", err))
		return "", CodexIdle
	}
	eligible, why := codexEligible(pol, installed)
	if why != "" {
		*errs = append(*errs, why)
	}
	if !eligible {
		return installed, CodexIdle
	}

	// One attempt per published version. A package that fails to install would
	// otherwise be re-downloaded and re-run every sync, burning bandwidth on
	// every machine in the ring.
	if s.readMarker(codexMarkerFile) == pol.CodexVersion {
		return installed, CodexFailed
	}

	if running, err := s.Codex.Running(); err != nil {
		*errs = append(*errs, fmt.Sprintf("codex: check whether it is running: %v", err))
		return installed, CodexDeferred
	} else if running {
		// Not an error: it will install the next time the machine is idle.
		return installed, CodexDeferred
	}
	if free, err := s.Codex.FreeBytes(); err == nil && free < installHeadroom {
		// Reported through the deferred state rather than as an error: the
		// machine is not broken, it just has no room today.
		log.Printf("codex: deferring %s, only %d MB free (need %d MB)",
			pol.CodexVersion, free>>20, uint64(installHeadroom)>>20)
		return installed, CodexDeferred
	}

	dest := filepath.Join(s.StateDir, "codex-setup.exe")
	log.Printf("codex: downloading %s", pol.CodexVersion)
	sum, err := s.Store.GetToFile(pol.CodexKey, dest)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("codex: download: %v", err))
		return installed, CodexFailed
	}
	if !strings.EqualFold(sum, pol.CodexSHA256) {
		// Refuse and delete: this is the check that stands between the fleet
		// and running whatever happened to be at that key.
		os.Remove(dest)
		*errs = append(*errs, fmt.Sprintf("codex: checksum mismatch (got %s, want %s); not installing",
			sum, pol.CodexSHA256))
		s.writeMarker(codexMarkerFile, pol.CodexVersion)
		return installed, CodexFailed
	}

	// Mark before installing, not after: an install that hangs or reboots the
	// machine must not come back and try again on the next sync.
	s.writeMarker(codexMarkerFile, pol.CodexVersion)
	log.Printf("codex: installing %s", pol.CodexVersion)
	if err := s.Codex.Install(dest, pol.CodexVersion); err != nil {
		*errs = append(*errs, fmt.Sprintf("codex: install: %v", err))
		return installed, CodexFailed
	}
	os.Remove(dest) // ~700 MB; the marker records what was done

	now, err := s.Codex.InstalledVersion()
	if err != nil || now == "" {
		now = pol.CodexVersion // installer reported success; trust it for this cycle
	}
	log.Printf("codex: installed %s", now)
	return now, CodexIdle
}
