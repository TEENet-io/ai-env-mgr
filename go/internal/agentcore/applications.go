package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

const (
	AppQueued           = "queued"
	AppDownloading      = "downloading"
	AppVerifying        = "verifying"
	AppDeferred         = "deferred"
	AppInstalling       = "installing"
	AppVerifyingInstall = "verifying_install"
	AppSucceeded        = "succeeded"
	AppRebootRequired   = "reboot_required"
	AppFailed           = "failed"
	AppBlocked          = "blocked"
	AppCancelled        = "cancelled"
)

// ApplicationInstaller contains the platform-specific machine-wide actions.
// It never accepts a free-form command: all arguments come from the approved
// application manifest passed to these methods.
type ApplicationInstaller interface {
	InstalledVersion(app model.Application) (string, error)
	Running(app model.Application) (bool, error)
	FreeBytes(app model.Application) (uint64, error)
	Install(app model.Application, setupPath string) error
	EnsureShortcut(app model.Application) (bool, error)
	LaunchAsStandardUser(app model.Application) (bool, error)
	AppLockerAllowed(app model.Application) (bool, error)
}

// ContextApplicationInstaller lets the Admin revoke an in-flight install.
type ContextApplicationInstaller interface {
	InstallContext(context.Context, model.Application, string) error
}

// updateApplications applies the desired machine application state. A single
// failed application does not prevent policy, credentials, or other apps from
// syncing. The returned statuses are included in the machine status report.
func (s *Syncer) updateApplications(machine string, errs *[]string) []model.ApplicationStatus {
	if s.Applications == nil {
		return nil
	}
	data, exists, err := s.source().Applications(machine)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("applications: read desired state: %v", err))
		return nil
	}
	if !exists {
		return nil
	}
	var desired model.MachineApplications
	if err := json.Unmarshal(data, &desired); err != nil {
		*errs = append(*errs, fmt.Sprintf("applications: parse desired state: %v", err))
		return nil
	}
	statuses := make([]model.ApplicationStatus, 0, len(desired.Apps))
	for _, item := range desired.Apps {
		if item.Desired != "installed" {
			// Cancelling a request also clears the local failure gate. Without
			// this, a later install of the same app could inherit a stale
			// marker forever and remain in "previous attempt failed".
			s.writeMarker(applicationFailureMarker(item.AppID), "")
			statuses = append(statuses, model.ApplicationStatus{
				AppID: item.AppID, DesiredVersion: item.Version, TaskID: item.TaskID,
				State: AppCancelled, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			})
			continue
		}
		if item.TaskID != "" && s.readMarker(applicationFailureMarker(item.AppID)) == item.TaskID {
			statuses = append(statuses, model.ApplicationStatus{
				AppID: item.AppID, DesiredVersion: item.Version, TaskID: item.TaskID,
				State: AppFailed, LastError: "previous attempt failed; create a new task to retry",
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			})
			continue
		}
		queued := model.ApplicationStatus{AppID: item.AppID, DesiredVersion: item.Version, TaskID: item.TaskID, State: AppQueued, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
		s.reportApplicationProgress(queued)
		st := s.applyApplication(item)
		if item.TaskID != "" {
			if st.State == AppFailed || st.State == AppBlocked {
				s.writeMarker(applicationFailureMarker(item.AppID), item.TaskID)
			} else if st.State == AppSucceeded || st.State == AppRebootRequired {
				s.writeMarker(applicationFailureMarker(item.AppID), "")
			}
		}
		statuses = append(statuses, st)
		if st.LastError != "" && st.State != AppDeferred {
			*errs = append(*errs, fmt.Sprintf("application %s: %s", item.AppID, st.LastError))
		}
	}
	return statuses
}

func (s *Syncer) reportApplicationProgress(st model.ApplicationStatus) {
	if s.ApplicationProgress != nil {
		s.ApplicationProgress(st)
	}
}

func applicationFailureMarker(appID string) string {
	return filepath.Join("applications", safeName(appID)+".failed")
}

func (s *Syncer) applyApplication(item model.DesiredApplication) model.ApplicationStatus {
	return s.applyApplicationContext(context.Background(), item, s.reportApplicationProgress)
}

// ExecuteApplication runs one Admin-leased task. No local failure marker is
// read or written; Admin owns retry, cancellation, and terminal state.
func (s *Syncer) ExecuteApplication(ctx context.Context, item model.DesiredApplication, progress func(model.ApplicationStatus)) model.ApplicationStatus {
	return s.applyApplicationContext(ctx, item, progress)
}

func (s *Syncer) applyApplicationContext(ctx context.Context, item model.DesiredApplication, progress func(model.ApplicationStatus)) model.ApplicationStatus {
	st := model.ApplicationStatus{
		AppID: item.AppID, DesiredVersion: item.Version, TaskID: item.TaskID,
		State: AppQueued, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := ctx.Err(); err != nil {
		return appFailure(st, AppCancelled, err.Error())
	}
	var data []byte
	var err error
	if item.TaskID != "" && item.LeaseToken != "" {
		if src, ok := s.source().(interface {
			ApplicationTask(context.Context, string, string, string, string) ([]byte, error)
		}); ok {
			data, err = src.ApplicationTask(ctx, item.AppID, item.Version, item.TaskID, item.LeaseToken)
		} else {
			return appFailure(st, AppFailed, "task-scoped application reads are not supported by this source")
		}
	} else {
		// Legacy OSS plans carry no lease token and continue through the
		// compatibility path. PostgreSQL-backed tasks must use the scoped path
		// above so a cancelled task cannot fetch a package.
		data, err = s.source().Application(item.AppID, item.Version)
	}
	if err != nil {
		return appFailure(st, AppFailed, fmt.Sprintf("read manifest: %v", err))
	}
	var app model.Application
	if err := json.Unmarshal(data, &app); err != nil {
		return appFailure(st, AppBlocked, fmt.Sprintf("parse manifest: %v", err))
	}
	if !strings.EqualFold(app.InstallerType, "msi") && !(strings.EqualFold(app.InstallerType, "exe") && model.IsVSCodeApplication(app)) {
		return appFailure(st, AppBlocked, "only MSI or the trusted VS Code installer is supported")
	}
	if !app.Enabled || !app.Approved || app.ObjectKey == "" || app.SHA256 == "" {
		return appFailure(st, AppBlocked, "manifest is not enabled, approved, and complete")
	}
	if app.ObjectKey != ossclient.ApplicationPackageKey(app.AppID, app.Version, app.InstallerType) {
		return appFailure(st, AppBlocked, "manifest package key is outside the approved application namespace")
	}

	installed, err := s.Applications.InstalledVersion(app)
	if err != nil {
		return appFailure(st, AppFailed, fmt.Sprintf("read installed version: %v", err))
	}
	st.InstalledVersion = installed
	if installed == app.Version {
		return s.finishApplication(st, app)
	}
	if installed != "" && !item.AllowDowngrade && installed != app.Version {
		// The platform adapter may return a version it can compare; the safe
		// default is to install only when the target differs, while never
		// deleting the existing application on a failed update.
	}
	if running, err := s.Applications.Running(app); err != nil {
		return appFailure(st, AppDeferred, fmt.Sprintf("check running state: %v", err))
	} else if running {
		return appFailure(st, AppDeferred, "application is running; waiting for it to close")
	}
	if free, err := s.Applications.FreeBytes(app); err == nil && app.Size > 0 && free < uint64(app.Size)+1<<30 {
		return appFailure(st, AppDeferred, fmt.Sprintf("not enough disk space: %d bytes free", free))
	}

	st.State = AppDownloading
	if progress != nil {
		progress(st)
	}
	destDir := filepath.Join(s.StateDir, "applications", safeName(app.AppID), safeName(app.Version))
	// Never reuse a fixed installer.exe name. On Windows a timed-out installer
	// can keep the old file open for a short while; a retry that downloads to
	// the same destination then fails its final rename with Access is denied.
	// Task-scoped names let the new download proceed independently and also
	// make the local files traceable to the Admin task that created them.
	dest := filepath.Join(destDir, installerFilename(app.InstallerType, item.TaskID))
	var sum string
	if item.TaskID != "" && item.LeaseToken != "" {
		if src, ok := s.source().(interface {
			ApplicationTaskToFileContext(context.Context, string, string, string, string, string) (string, error)
		}); ok {
			sum, err = src.ApplicationTaskToFileContext(ctx, app.AppID, app.Version, item.TaskID, item.LeaseToken, dest)
		} else {
			return appFailure(st, AppFailed, "task-scoped application downloads are not supported by this source")
		}
	} else if src, ok := s.source().(interface {
		ApplicationToFileContext(context.Context, string, string, string) (string, error)
	}); ok {
		sum, err = src.ApplicationToFileContext(ctx, app.AppID, app.Version, dest)
	} else {
		sum, err = s.source().ApplicationToFile(app.AppID, app.Version, dest)
	}
	if err != nil {
		if ctx.Err() != nil {
			return appFailure(st, AppCancelled, ctx.Err().Error())
		}
		return appFailure(st, AppFailed, fmt.Sprintf("download: %v", err))
	}
	st.State = AppVerifying
	if progress != nil {
		progress(st)
	}
	if !strings.EqualFold(sum, app.SHA256) {
		_ = os.Remove(dest)
		return appFailure(st, AppBlocked, fmt.Sprintf("sha256 mismatch: got %s", sum))
	}
	st.State = AppInstalling
	if progress != nil {
		progress(st)
	}
	if err := ctx.Err(); err != nil {
		return appFailure(st, AppCancelled, err.Error())
	}
	if installer, ok := s.Applications.(ContextApplicationInstaller); ok {
		err = installer.InstallContext(ctx, app, dest)
	} else {
		err = s.Applications.Install(app, dest)
	}
	if err != nil {
		if ctx.Err() != nil {
			return appFailure(st, AppCancelled, ctx.Err().Error())
		}
		return appFailure(st, AppFailed, fmt.Sprintf("install: %v", err))
	}
	_ = os.Remove(dest)
	if err := ctx.Err(); err != nil {
		return appFailure(st, AppCancelled, err.Error())
	}
	st.State = AppVerifyingInstall
	return s.finishApplication(st, app)
}

func installerFilename(installerType, taskID string) string {
	suffix := safeName(taskID)
	if suffix == "_invalid" || suffix == "." {
		suffix = "current"
	}
	ext := ".msi"
	if strings.EqualFold(installerType, "exe") {
		ext = ".exe"
	}
	return "installer-" + suffix + ext
}

func (s *Syncer) finishApplication(st model.ApplicationStatus, app model.Application) model.ApplicationStatus {
	shortcut, err := s.Applications.EnsureShortcut(app)
	if err != nil {
		return appFailure(st, AppFailed, fmt.Sprintf("desktop shortcut: %v", err))
	}
	st.PublicDesktopShortcut = shortcut
	launch, err := s.Applications.LaunchAsStandardUser(app)
	if err != nil {
		return appFailure(st, AppFailed, fmt.Sprintf("standard-user launch check: %v", err))
	}
	st.LaunchAsStandardUser = launch
	allowed, err := s.Applications.AppLockerAllowed(app)
	if err != nil {
		return appFailure(st, AppBlocked, fmt.Sprintf("AppLocker check: %v", err))
	}
	st.AppLockerAllowed = allowed
	if !shortcut || !launch {
		return appFailure(st, AppFailed, "application installed but is not usable from the employee desktop")
	}
	if !allowed {
		return appFailure(st, AppBlocked, "AppLocker would block the application")
	}
	st.InstalledVersion = app.Version
	st.RebootRequired = app.RequiresReboot
	if app.RequiresReboot {
		st.State = AppRebootRequired
	} else {
		st.State = AppSucceeded
	}
	st.LastError = ""
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return st
}

func appFailure(st model.ApplicationStatus, state, message string) model.ApplicationStatus {
	st.State, st.LastError = state, message
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return st
}

func safeName(v string) string {
	v = filepath.Base(filepath.Clean(v))
	if v == "." || v == string(filepath.Separator) || v == "" {
		return "_invalid"
	}
	return v
}
