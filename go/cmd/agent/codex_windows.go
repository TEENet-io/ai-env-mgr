//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
)

// codexInstaller is the Windows half of the Codex desktop update.
//
// The image installs Codex machine-wide under codexRoot rather than into a
// user profile, which is what makes this possible at all: the agent runs as
// SYSTEM, and a per-user install would land in SYSTEM's own profile where no
// employee would ever see it.
type codexInstaller struct {
	root string // where Codex lives, e.g. C:\tools\Codex
}

// codexDefaultRoot is where the desktop image puts Codex. Overridable so a
// machine that differs does not need a new binary.
const codexDefaultRoot = `C:\tools\Codex`

func newCodexInstaller() agentcore.CodexInstaller {
	root := os.Getenv("AIENVMGR_CODEX_ROOT")
	if root == "" {
		root = codexDefaultRoot
	}
	return &codexInstaller{root: root}
}

// codexVersionFile records what the agent installed.
//
// The installer's own uninstall registry entry is the more authoritative
// source, but Inno Setup writes it under HKCU for a per-user install and HKLM
// for a machine-wide one, and the image was not necessarily produced by the
// installer at all. A file beside the application answers the question the
// same way regardless, and is what the agent itself wrote.
const codexVersionFile = "aienvmgr-version.txt"

// codexUninstallKeys are where Inno Setup records a machine-wide install. The
// AppId comes from installer/CodexOffline.iss.tpl in the codex-kiosk repo;
// changing it there means changing it here.
var codexUninstallKeys = []string{
	`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\{A68E32B0-4AA6-4B16-9364-B668731F7062}_is1`,
	`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\{A68E32B0-4AA6-4B16-9364-B668731F7062}_is1`,
}

func (c *codexInstaller) InstalledVersion() (string, error) {
	// What the agent wrote wins: it describes the package this fleet manages,
	// including one installed over an image that had no registry entry.
	if b, err := os.ReadFile(filepath.Join(c.root, codexVersionFile)); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
	}
	for _, path := range codexUninstallKeys {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, err := k.GetStringValue("DisplayVersion")
		k.Close()
		if err == nil && v != "" {
			return v, nil
		}
	}
	// Not installed, or installed by something that left no trace. Either way
	// the published version differs, so it will be installed.
	return "", nil
}

// codexProcesses are the executables that mean Codex is in use. The repackaged
// app still ships Electron's original ChatGPT.exe as its main binary.
var codexProcesses = []string{"ChatGPT.exe", "Codex.exe"}

func (c *codexInstaller) Running() (bool, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snap)

	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return false, err
	}
	for {
		name := windows.UTF16ToString(e.ExeFile[:])
		for _, want := range codexProcesses {
			if strings.EqualFold(name, want) {
				return true, nil
			}
		}
		if err := windows.Process32Next(snap, &e); err != nil {
			return false, nil // ERROR_NO_MORE_FILES: end of the list
		}
	}
}

func (c *codexInstaller) FreeBytes() (uint64, error) {
	// Ask about the volume Codex lives on, which need not be the system drive.
	vol := filepath.VolumeName(c.root)
	if vol == "" {
		vol = "C:"
	}
	p, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

// Install runs the Inno Setup package silently into the existing location.
//
// /DIR pins it to where Codex already is, so an update never silently moves
// the application: shortcuts, the agent's own version file, and anything else
// pointing at the old path would be left behind.
func (c *codexInstaller) Install(setupPath, version string) error {
	logPath := filepath.Join(filepath.Dir(setupPath), "codex-install.log")
	cmd := exec.Command(setupPath,
		"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/NOCANCEL",
		"/DIR="+c.root,
		"/LOG="+logPath,
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start installer: %w", err)
	}

	// Bound the wait. A silent installer that hangs must not hold the sync
	// loop -- and with it heartbeats -- for the rest of the machine's uptime.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			// Inno Setup exit codes: 1 setup failed, 2/5 cancelled,
			// 1641/3010 success but a restart is needed.
			if code := cmd.ProcessState.ExitCode(); code == 1641 || code == 3010 {
				break
			}
			return fmt.Errorf("installer exited %d (log: %s): %w",
				cmd.ProcessState.ExitCode(), logPath, err)
		}
	case <-time.After(20 * time.Minute):
		_ = cmd.Process.Kill()
		return fmt.Errorf("installer did not finish within 20 minutes (log: %s)", logPath)
	}

	// Record what was installed, so InstalledVersion can answer without
	// depending on how the installer registered itself.
	if err := os.MkdirAll(c.root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.root, codexVersionFile), []byte(version), 0o644)
}
