//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

type applicationInstaller struct{}

func newApplicationInstaller() agentcore.ApplicationInstaller { return applicationInstaller{} }

func (applicationInstaller) InstalledVersion(app model.Application) (string, error) {
	if _, err := os.Stat(app.Detection.Path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if app.Detection.Type == "file_exists" || app.Detection.Version == "" {
		return app.Version, nil
	}
	// VersionInfo is read through PowerShell's fixed Get-Item cmdlet. The path
	// is escaped as a literal path and comes only from the approved manifest.
	path := psQuote(app.Detection.Path)
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-Item -LiteralPath '"+path+"').VersionInfo.ProductVersion").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (applicationInstaller) Running(app model.Application) (bool, error) {
	name := strings.ToLower(filepath.Base(app.Detection.Path))
	if name == "." || name == "" {
		return false, nil
	}
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
		if strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), name) {
			return true, nil
		}
		if err := windows.Process32Next(snap, &e); err != nil {
			return false, nil
		}
	}
}

func (applicationInstaller) FreeBytes(app model.Application) (uint64, error) {
	vol := filepath.VolumeName(app.Detection.Path)
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

func (applicationInstaller) Install(app model.Application, setupPath string) error {
	return applicationInstaller{}.InstallContext(context.Background(), app, setupPath)
}

func (applicationInstaller) InstallContext(parent context.Context, app model.Application, setupPath string) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if strings.EqualFold(app.InstallerType, "msi") {
		// MSI has a standard unattended invocation. Optional manifest arguments
		// remain available for vendor properties.
		args := []string{"/i", setupPath, "/qn", "/norestart"}
		args = append(args, app.SilentArgs...)
		cmd = exec.CommandContext(ctx, "msiexec.exe", args...)
	} else if strings.EqualFold(app.InstallerType, "exe") && model.IsVSCodeApplication(app) {
		// The only trusted EXE template. Do not accept manifest-provided
		// switches here: VS Code's Inno Setup switches are fixed and silent.
		args := append([]string{}, model.VSCodeSilentArgs()...)
		// Keep the vendor's diagnostic log beside the task-scoped installer so
		// an exit code such as 1 is actionable instead of opaque. The log is
		// removed after a successful install and retained on failure.
		args = append(args, "/LOG="+setupPath+".log")
		cmd = exec.CommandContext(ctx, setupPath, args...)
	} else {
		return fmt.Errorf("only MSI or the trusted VS Code installer is supported")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start installer: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		if err != nil && code != 1641 && code != 3010 {
			detail := installerDiagnostics(setupPath+".log", stdout.String(), stderr.String())
			if detail != "" {
				return fmt.Errorf("installer exited %d: %w (%s)", code, err, detail)
			}
			return fmt.Errorf("installer exited %d: %w", code, err)
		}
	case <-ctx.Done():
		killProcessTree(cmd.Process.Pid)
		// Reap the child before returning. A GUI installer that was launched
		// without silent arguments can otherwise survive the timeout and keep
		// the old executable open while the next task starts.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		if parent.Err() != nil {
			return parent.Err()
		}
		return fmt.Errorf("installer did not finish within 30 minutes; configure silent installer arguments for non-interactive installation")
	}
	_ = os.Remove(setupPath + ".log")
	return nil
}

func installerDiagnostics(logPath, stdout, stderr string) string {
	if data, err := os.ReadFile(logPath); err == nil && len(data) > 0 {
		const max = 8 << 10
		if len(data) > max {
			data = data[len(data)-max:]
		}
		return strings.TrimSpace(string(data))
	}
	if strings.TrimSpace(stderr) != "" {
		return strings.TrimSpace(stderr)
	}
	return strings.TrimSpace(stdout)
}

func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = exec.Command("taskkill.exe", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

func (applicationInstaller) EnsureShortcut(app model.Application) (bool, error) {
	if !app.Shortcut.Enabled {
		return true, nil
	}
	if !app.Shortcut.PublicDesktop {
		return false, fmt.Errorf("only public desktop shortcuts are supported")
	}
	public := os.Getenv("PUBLIC")
	if public == "" {
		public = `C:\Users\Public`
	}
	link := filepath.Join(public, "Desktop", app.Shortcut.Name+".lnk")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return false, err
	}
	cmd := "$w=New-Object -ComObject WScript.Shell; $s=$w.CreateShortcut('" + psQuote(link) + "'); $s.TargetPath='" + psQuote(app.Shortcut.Target) + "';"
	if app.Shortcut.WorkingDirectory != "" {
		cmd += "$s.WorkingDirectory='" + psQuote(app.Shortcut.WorkingDirectory) + "';"
	}
	if app.Shortcut.Icon != "" {
		cmd += "$s.IconLocation='" + psQuote(app.Shortcut.Icon) + "';"
	}
	cmd += "$s.Save()"
	if out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmd).CombinedOutput(); err != nil {
		return false, fmt.Errorf("create public desktop shortcut: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_, err := os.Stat(link)
	return err == nil, err
}

func (applicationInstaller) LaunchAsStandardUser(app model.Application) (bool, error) {
	// A SYSTEM service must not launch the employee's application. Verify the
	// executable is present and readable; the shortcut is what the employee
	// launches in their own session.
	target := app.Shortcut.Target
	if target == "" {
		target = app.Detection.Path
	}
	info, err := os.Stat(target)
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}

func (applicationInstaller) AppLockerAllowed(app model.Application) (bool, error) {
	// Ask AppLocker for a decision without starting the application. If the
	// machine is not enforcing AppLocker, it cannot block this launch.
	target := app.Shortcut.Target
	if target == "" {
		target = app.Detection.Path
	}
	path := psQuote(target)
	cmd := "$p=Get-AppLockerPolicy -Effective; if ($null -eq $p) { 'true'; exit }; $f=Get-AppLockerFileInformation -Path '" + path + "'; $r=Test-AppLockerPolicy -PolicyObject $p -FileInformation $f; $d=[string]$r.PolicyDecision; if ($d -eq 'Allowed' -or $d -eq 'AllowedByDefault') {'true'} else {'false'}"
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmd).Output()
	if err != nil {
		// On older Windows images the cmdlet can be unavailable. Returning an
		// error blocks the task instead of claiming an unsafe install succeeded.
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(string(out)), "true"), nil
}

func psQuote(v string) string { return strings.ReplaceAll(v, "'", "''") }
