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

// Why an eligible Codex install waited, reported beside CodexDeferred.
const (
	CodexDeferInUse = "in_use"
	CodexDeferDisk  = "disk"
)

// codexOutcome is what one Codex update cycle reports.
type codexOutcome struct {
	Version string // what is installed now
	State   string // one of the Codex* states
	Reason  string // for CodexDeferred: one of the CodexDefer* reasons
}

// codexMarkerFile records the last version this machine attempted, so a
// package that fails to install is not retried every sync forever. Clearing
// the target, or publishing a different version, makes it try again.
const codexMarkerFile = "codex-target"

// installHeadroom is what must be free before downloading: the installer
// itself plus room for what it unpacks. A cloud desktop that fills its disk
// mid-install is a worse outcome than an update that waits.
const installHeadroom = 3 << 30 // 3 GiB

// codexEligible reports whether this machine should take the targeted Codex
// version, and why not when it should not.
//
// The comparison is for EQUALITY, never "newer". Publishing an older version
// is how a bad build is rolled back, and an upgrade-only rule cannot express
// that -- which is precisely when it would be needed.
func codexEligible(target model.ReleaseTarget, installed string) (bool, string) {
	if target.Version == "" {
		return false, "" // nothing targeted; also the kill switch
	}
	if target.Key == "" || target.SHA256 == "" {
		return false, "codex: targeted version has no object key or checksum"
	}
	if installed == target.Version {
		return false, ""
	}
	return true, ""
}

// updateCodex runs one Codex update cycle: the version now installed, the
// state to report and, for a deferral, the reason. Problems go to errs.
//
// Nothing here is fatal to the sync: a machine that cannot update Codex still
// has to report its status, apply policy, and stay manageable.
func (s *Syncer) updateCodex(target model.ReleaseTarget, errs *[]string) codexOutcome {
	if s.Codex == nil {
		return codexOutcome{}
	}
	installed, err := s.Codex.InstalledVersion()
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("codex: read installed version: %v", err))
		return codexOutcome{}
	}
	eligible, why := codexEligible(target, installed)
	if why != "" {
		*errs = append(*errs, why)
	}
	if !eligible {
		return codexOutcome{Version: installed}
	}

	// One attempt per generation of a target. A package that fails to install
	// would otherwise be re-downloaded and re-run every sync, burning
	// bandwidth on every machine; the console retries by moving the
	// generation, which is a decision somebody made.
	if markerMatches(s.readMarker(codexMarkerFile), target) {
		return codexOutcome{Version: installed, State: CodexFailed}
	}

	if running, err := s.Codex.Running(); err != nil {
		*errs = append(*errs, fmt.Sprintf("codex: check whether it is running: %v", err))
		return codexOutcome{Version: installed, State: CodexDeferred, Reason: CodexDeferInUse}
	} else if running {
		// Not an error: it will install the next time the machine is idle.
		return codexOutcome{Version: installed, State: CodexDeferred, Reason: CodexDeferInUse}
	}
	if free, err := s.Codex.FreeBytes(); err == nil && free < installHeadroom {
		// Reported through the deferred state rather than as an error: the
		// machine is not broken, it just has no room today.
		log.Printf("codex: deferring %s, only %d MB free (need %d MB)",
			target.Version, free>>20, uint64(installHeadroom)>>20)
		return codexOutcome{Version: installed, State: CodexDeferred, Reason: CodexDeferDisk}
	}

	dest := filepath.Join(s.StateDir, "codex-setup.exe")
	log.Printf("codex: downloading %s", target.Version)
	sum, err := s.source().ArtifactToFile(ProductCodex, target, dest)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("codex: download: %v", err))
		return codexOutcome{Version: installed, State: CodexFailed}
	}
	if !strings.EqualFold(sum, target.SHA256) {
		// Refuse and delete: this is the check that stands between the fleet
		// and running whatever happened to be at that key.
		os.Remove(dest)
		*errs = append(*errs, fmt.Sprintf("codex: checksum mismatch (got %s, want %s); not installing",
			sum, target.SHA256))
		s.writeMarker(codexMarkerFile, targetMarker(target))
		return codexOutcome{Version: installed, State: CodexFailed}
	}

	// Mark before installing, not after: an install that hangs or reboots the
	// machine must not come back and try again on the next sync.
	s.writeMarker(codexMarkerFile, targetMarker(target))
	log.Printf("codex: installing %s", target.Version)
	if err := s.Codex.Install(dest, target.Version); err != nil {
		*errs = append(*errs, fmt.Sprintf("codex: install: %v", err))
		return codexOutcome{Version: installed, State: CodexFailed}
	}
	os.Remove(dest) // ~700 MB; the marker records what was done

	now, err := s.Codex.InstalledVersion()
	if err != nil || now == "" {
		now = target.Version // installer reported success; trust it for this cycle
	}
	log.Printf("codex: installed %s", now)
	return codexOutcome{Version: now, State: CodexIdle}
}
