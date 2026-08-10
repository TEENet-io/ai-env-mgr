//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// localUpdater replaces the running agent.exe and restarts the service.
//
// On Windows a running executable can be renamed (the handle stays valid) but
// not overwritten, so the current exe is moved aside to agent.exe.old, the new
// binary takes its place, and the service is restarted so the Service Control
// Manager launches the new file. agent.exe.old is kept for manual rollback.
type localUpdater struct{}

func (localUpdater) ApplyUpdate(newBinary []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current exe: %w", err)
	}
	dir := filepath.Dir(exe)
	newPath := filepath.Join(dir, "agent.exe.new")
	oldPath := filepath.Join(dir, "agent.exe.old")

	if err := os.WriteFile(newPath, newBinary, 0o755); err != nil {
		return fmt.Errorf("write new binary: %w", err)
	}
	_ = os.Remove(oldPath)
	if err := os.Rename(exe, oldPath); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("move current exe aside: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		_ = os.Rename(oldPath, exe) // best-effort revert
		return fmt.Errorf("put new exe in place: %w", err)
	}

	return restartServiceDetached(serviceName)
}

// restartServiceDetached spawns a short-lived, detached helper that stops and
// starts the service after a brief delay. It has to be a separate process
// because the service cannot stop-then-start itself in-process; DETACHED_PROCESS
// keeps the helper alive across the stop.
func restartServiceDetached(svc string) error {
	cmd := exec.Command("cmd.exe", "/c",
		"timeout /t 3 /nobreak >nul & sc stop "+svc+" & sc start "+svc)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("schedule service restart: %w", err)
	}
	return nil
}
