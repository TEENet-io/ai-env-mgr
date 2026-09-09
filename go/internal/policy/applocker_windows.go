//go:build windows

package policy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf16"
)

// ErrAppLockerNotDeployed means this machine has no AppLocker policy with an
// Exe rule collection. The agent never creates one -- whether AppLocker is
// deployed at all is the image's decision -- so callers should report this
// as a warning, not a failure.
var ErrAppLockerNotDeployed = errors.New("AppLocker is not deployed on this machine")

// readLocalAppLockerXML runs Get-AppLockerPolicy -Local -Xml and returns the
// document with any UTF-8 BOM and surrounding whitespace stripped. This is
// the only place that shells out to read the local AppLocker policy; both
// ApplyAppLocker and LocalAppLockerPaths call it, so there is a single spot
// that knows how the policy is actually read off the machine.
func readLocalAppLockerXML() (string, error) {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", "Import-Module AppLocker; Get-AppLockerPolicy -Local -Xml").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff")), nil
}

// LocalAppLockerPaths returns the paths of the AppLocker rules the agent
// currently manages on this machine, read directly from its local AppLocker
// policy (see ManagedPaths) -- not from any published policy.json. It
// returns an error only when the local policy could not be read at all; a
// machine with no AppLocker deployment, or a deployed policy with no managed
// rules, both report (nil, nil), because "zero rules" is a fact this
// function was able to establish, not a failure.
//
// Callers that need to know what is actually applied -- as opposed to
// ApplyAppLocker, which already knows the desired paths from the policy
// object it was given -- must be able to tell "confirmed zero" from "could
// not read" apart, which is exactly what the error return is for.
func LocalAppLockerPaths() ([]string, error) {
	xml, err := readLocalAppLockerXML()
	if err != nil {
		return nil, fmt.Errorf("read local AppLocker policy: %w", err)
	}
	return ManagedPaths(xml), nil
}

// ApplyAppLocker brings the machine's LOCAL AppLocker policy in line with
// paths (see RewriteAppLockerXML).
//
// It reads the LOCAL policy rather than the effective one: the effective
// policy folds in domain GPOs, and writing that back would copy someone
// else's rules into our local store, where they would then outlive the GPO.
//
// It is called on every sync cycle, so it must stay cheap and quiet when
// nothing has drifted: one read, and a write only when the XML actually
// changes.
func ApplyAppLocker(paths []string) error {
	// policy.json is written on the publishing side, not here, and this
	// agent runs as SYSTEM: re-validate before anything reaches the
	// rewriter rather than trusting whatever OSS happened to hand back. See
	// FilterAllowPaths.
	paths, rejected := FilterAllowPaths(paths)

	xml, err := readLocalAppLockerXML()
	if err != nil {
		if len(paths) == 0 {
			// Nothing valid to manage and nothing readable: not worth a
			// status line of its own, but a dropped entry still is.
			return rejectedErr(rejected)
		}
		return fmt.Errorf("read local AppLocker policy: %w", err)
	}
	if !AppLockerDeployed(xml) {
		if len(paths) == 0 {
			return rejectedErr(rejected)
		}
		return fmt.Errorf("%w; %d allow path(s) not applied", ErrAppLockerNotDeployed, len(paths))
	}
	next, changed, err := RewriteAppLockerXML(xml, paths)
	if err != nil {
		return fmt.Errorf("rewrite AppLocker policy: %w", err)
	}
	if !changed {
		return rejectedErr(rejected)
	}
	// Staged in a directory only SYSTEM and Administrators can write, under
	// an unpredictable name created with O_EXCL: see SetAppLockerStagingDir.
	dir, err := appLockerStagingDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create AppLocker staging directory %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "applocker-*.xml")
	if err != nil {
		return fmt.Errorf("stage AppLocker policy: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, werr := f.Write(utf16LEWithBOM(next)); werr != nil {
		f.Close()
		return fmt.Errorf("write staged AppLocker policy: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("write staged AppLocker policy: %w", cerr)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", fmt.Sprintf("Import-Module AppLocker; Set-AppLockerPolicy -XmlPolicy '%s' -ErrorAction Stop", tmp))
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Set-AppLockerPolicy: %v: %s", err, strings.TrimSpace(string(msg)))
	}
	return rejectedErr(rejected)
}

// utf16LEWithBOM encodes the way the image script writes the policy file
// (Out-File -Encoding Unicode), which Set-AppLockerPolicy reads reliably.
func utf16LEWithBOM(s string) []byte {
	u := utf16.Encode([]rune(s))
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xFE})
	for _, r := range u {
		b.WriteByte(byte(r))
		b.WriteByte(byte(r >> 8))
	}
	return b.Bytes()
}
