package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/winsvc"
)

// installRoot is where the agent lives once installed. Under Program Files
// because a standard user cannot write there, so an employee cannot swap the
// binary for one pointing at a bucket of their choosing.
const installRoot = `C:\Program Files\AIEnvMgr`

// cmdSetup does everything needed to turn a freshly downloaded agent.exe into
// a running service, in one step.
//
// This exists because the manual sequence -- copy, lock down, register,
// start, verify -- is five commands where forgetting the third silently
// leaves the OSS key readable by every employee on the machine. It runs once,
// on the template machine, before the image is taken; every desktop created
// from that image already has the service registered.
//
// It is safe to re-run: each step checks the current state first.
func cmdSetup() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("setup is only meaningful on Windows")
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}
	self, _ = filepath.Abs(self)
	target := filepath.Join(installRoot, "agent.exe")

	// 1. Put the binary in place.
	if strings.EqualFold(self, target) {
		fmt.Printf("[1/5] already running from %s\n", target)
	} else {
		if err := os.MkdirAll(installRoot, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", installRoot, err)
		}
		// The service may be running from the target, holding the file open.
		if running, _ := winsvc.IsRunning(serviceName); running {
			fmt.Println("[1/5] stopping the running service to replace the binary")
			if err := winsvc.Stop(serviceName); err != nil {
				return fmt.Errorf("stop the existing service: %w", err)
			}
			waitForStop()
		}
		if err := copyFile(self, target); err != nil {
			return fmt.Errorf("copy to %s: %w", target, err)
		}
		fmt.Printf("[1/5] copied to %s\n", target)
	}

	// 2. Lock the binary down. The OSS key is inside it, so a standard user
	// must not be able to read it -- this is the step most easily forgotten
	// when the sequence is run by hand.
	if err := restrictAccess(target); err != nil {
		return fmt.Errorf("restrict access to %s: %w", target, err)
	}
	fmt.Println("[2/5] permissions restricted to SYSTEM and Administrators")

	// 3. Register the service.
	installed, err := winsvc.IsInstalled(serviceName)
	if err != nil {
		return err
	}
	if installed {
		fmt.Printf("[3/5] service %q already registered\n", serviceName)
	} else {
		if err := winsvc.Install(serviceName, serviceDisp, serviceDesc); err != nil {
			return err
		}
		fmt.Printf("[3/5] service %q registered (starts automatically at boot)\n", serviceName)
	}

	// 4. Start it.
	if running, _ := winsvc.IsRunning(serviceName); running {
		fmt.Println("[4/5] service already running")
	} else {
		if err := winsvc.Start(serviceName); err != nil {
			return err
		}
		fmt.Println("[4/5] service started")
	}

	// 5. Prove it works rather than assuming it does. This runs a sync in
	// this process, which is the same code path the service runs.
	fmt.Println("[5/5] verifying...")
	fmt.Println()
	st, err := runOnce()
	if err != nil {
		return fmt.Errorf("verification sync failed: %w", err)
	}
	printStatus(st, false)

	fmt.Println()
	summarise(st.BlockEnabled, st.BlockedDomains, st.AppLockerMode)
	return nil
}

// summarise turns the verification result into the two or three things worth
// acting on, so the operator does not have to interpret the status dump.
func summarise(blockEnabled bool, domains int, appLocker string) {
	var problems []string
	if !blockEnabled || domains == 0 {
		problems = append(problems,
			"NO BLOCK IS IN FORCE. Run 'admin.exe enable-block' first, then re-run setup.")
	}
	switch appLocker {
	case "None":
		problems = append(problems,
			"AppLocker is not enabled, so portable browsers are not blocked.")
	case "Audit":
		problems = append(problems,
			"AppLocker is in audit mode: it logs but does not block. Switch to enforce before imaging.")
	}

	if len(problems) == 0 {
		fmt.Println("setup complete. The service will keep this machine in sync from now on.")
		fmt.Println("Take the image once you have checked the browser block by hand.")
		return
	}
	fmt.Println("setup finished, but check these before taking the image:")
	for _, p := range problems {
		fmt.Println("  -", p)
	}
}

// waitForStop gives the service control manager a moment to release the
// binary. Replacing a file Windows still has open fails outright.
func waitForStop() {
	for i := 0; i < 20; i++ {
		if running, _ := winsvc.IsRunning(serviceName); !running {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// restrictAccess strips inherited permissions and leaves only SYSTEM and the
// administrators group, so a standard user cannot read the embedded key.
func restrictAccess(path string) error {
	cmd := exec.Command("icacls", path,
		"/inheritance:r",
		"/grant", "SYSTEM:(F)",
		"/grant", "Administrators:(F)")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("icacls: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
