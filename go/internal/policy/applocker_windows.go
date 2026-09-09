//go:build windows

package policy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// ApplyAppLocker and Current (registry_windows.go) call it, so there is a
// single spot that knows how the policy is actually read off the machine.
func readLocalAppLockerXML() (string, error) {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", "Import-Module AppLocker; Get-AppLockerPolicy -Local -Xml").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff")), nil
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
	xml, err := readLocalAppLockerXML()
	if err != nil {
		if len(paths) == 0 {
			// Nothing to manage and nothing readable: not worth a status line.
			return nil
		}
		return fmt.Errorf("read local AppLocker policy: %w", err)
	}
	if !AppLockerDeployed(xml) {
		if len(paths) == 0 {
			return nil
		}
		return fmt.Errorf("%w; %d allow path(s) not applied", ErrAppLockerNotDeployed, len(paths))
	}
	next, changed, err := RewriteAppLockerXML(xml, paths)
	if err != nil {
		return fmt.Errorf("rewrite AppLocker policy: %w", err)
	}
	if !changed {
		return nil
	}
	tmp := filepath.Join(os.TempDir(), "aienvmgr-applocker.xml")
	if err := os.WriteFile(tmp, utf16LEWithBOM(next), 0o600); err != nil {
		return fmt.Errorf("write AppLocker policy temp file: %w", err)
	}
	defer os.Remove(tmp)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", fmt.Sprintf("Import-Module AppLocker; Set-AppLockerPolicy -XmlPolicy '%s' -ErrorAction Stop", tmp))
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Set-AppLockerPolicy: %v: %s", err, strings.TrimSpace(string(msg)))
	}
	return nil
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
