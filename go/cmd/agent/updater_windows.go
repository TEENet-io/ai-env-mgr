//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/winsvc"
)

// localUpdater stages the new image and starts a copy of the agent as a
// detached updater. The copy is a different executable file, so it remains
// runnable while the service image is stopped and replaced. This is the same
// shape as Codex/software updates (an installer child process owns the
// replacement), and avoids a process trying to rename its own image.
type localUpdater struct{}

func (localUpdater) ApplyUpdate(newBinary []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current exe: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve current exe: %w", err)
	}
	dir := filepath.Dir(exe)
	suffix := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	staged := filepath.Join(dir, "agent.exe.new-"+suffix)
	backup := filepath.Join(dir, "agent.exe.old-"+suffix)
	helper := filepath.Join(dir, "agent-updater-"+suffix+".exe")

	if err := writeExecutable(staged, newBinary); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	// The helper is a copy, not the service image. It can therefore keep
	// running after it asks the Service Control Manager to stop this process.
	if err := copyFile(exe, helper); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("stage updater helper: %w", err)
	}

	cmd := exec.Command(helper, "--update-helper", serviceName, exe, staged, backup)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(staged)
		_ = os.Remove(helper)
		return fmt.Errorf("start updater helper: %w", err)
	}
	return nil
}

func writeExecutable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

// runUpdateHelper is called from a copied agent.exe. The helper owns the
// stop/wait/swap/start sequence and writes a result for the next Agent startup
// to include in its local log. No shell is involved, so paths cannot be
// reinterpreted as commands and spaces in C:\\Program Files are safe.
func runUpdateHelper(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("update helper: want service, current, staged, backup")
	}
	svc, current, staged, backup := args[0], args[1], args[2], args[3]
	if svc == "" || current == "" || staged == "" || backup == "" {
		return fmt.Errorf("update helper: empty argument")
	}
	current, staged, backup = filepath.Clean(current), filepath.Clean(staged), filepath.Clean(backup)
	if filepath.Dir(current) != filepath.Dir(staged) || filepath.Dir(current) != filepath.Dir(backup) {
		return fmt.Errorf("update helper: files must be in one directory")
	}

	if err := winsvc.Stop(svc); err != nil {
		return updateHelperFailure(err)
	}
	// SERVICE_STOPPED can be reported before the process has released its
	// image section. Retry the rename itself; checking only the SCM state races
	// the last few milliseconds of process teardown.
	var moveErr error
	for i := 0; i < 120; i++ {
		if err := os.Rename(current, backup); err == nil {
			moveErr = nil
			break
		} else {
			moveErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	if moveErr != nil {
		_ = winsvc.Start(svc)
		return updateHelperFailure(fmt.Errorf("move current exe aside: %w", moveErr))
	}
	if err := os.Rename(staged, current); err != nil {
		_ = os.Rename(backup, current)
		_ = winsvc.Start(svc)
		return updateHelperFailure(fmt.Errorf("put new exe in place: %w", err))
	}
	if err := winsvc.Start(svc); err != nil {
		// Keep the old image until the replacement has at least been handed
		// back to the service manager. This leaves a recovery copy when the
		// new service cannot be started; a successful startup removes it.
		return updateHelperFailure(fmt.Errorf("start service after update: %w", err))
	}
	// The backup exists only long enough to make the swap reversible. We do
	// not retain old Agent binaries on the machine after the new image is in
	// place; cleanup is retried below in case antivirus briefly holds the file.
	backupErr := removeUpdateBackup(backup)
	if backupErr != nil {
		return writeUpdateHelperResult("updated; old backup cleanup pending: " + backupErr.Error())
	}
	return writeUpdateHelperResult("updated")
}

func removeUpdateBackup(path string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Remove(path); err == nil || os.IsNotExist(err) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

func updateHelperFailure(err error) error {
	_ = writeUpdateHelperResult("failed: " + err.Error())
	return err
}

func writeUpdateHelperResult(result string) error {
	path := filepath.Join(stateDir(), "agent-update-result.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(result+"\n"), 0o600)
}

// consumeUpdateHelperResult is intentionally best-effort. The helper may be
// unable to write when the machine is shutting down, but that must not prevent
// the new Agent from starting.
func consumeUpdateHelperResult() string {
	path := filepath.Join(stateDir(), "agent-update-result.txt")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	_ = os.Remove(path)
	return strings.TrimSpace(string(b))
}

// cleanupUpdateHelpers removes copied updater images and legacy rollback files
// after the new service has started. The first attempt may race the helper's
// final exit or an antivirus scan, so retry for a short period; failed removes
// are harmless and never affect the service.
func cleanupUpdateHelpers() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	for attempt := 0; attempt < 30; attempt++ {
		entries, err := os.ReadDir(dir)
		if err == nil {
			left := false
			for _, entry := range entries {
				name := entry.Name()
				if (strings.HasPrefix(name, "agent-updater-") && strings.HasSuffix(name, ".exe")) ||
					strings.HasPrefix(name, "agent.exe.old-") || name == "agent.exe.old" {
					left = true
					_ = os.Remove(filepath.Join(dir, name))
				}
			}
			if !left {
				return
			}
		}
		time.Sleep(time.Second)
	}
}
