package adminweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

var packageVersionRE = regexp.MustCompile(`(?i)(\d+(?:\.\d+){1,3})`)

func inferApplication(r *http.Request, app *model.Application) {
	source := ""
	if f, h, err := r.FormFile("file"); err == nil {
		source = h.Filename
		_ = f.Close()
	}
	if source == "" {
		if raw := formValue(r, "url"); raw != "" {
			if u, err := url.Parse(raw); err == nil {
				source = path.Base(u.Path)
			}
		}
	}
	name := strings.TrimSuffix(strings.TrimSuffix(path.Base(strings.ReplaceAll(source, `\`, "/")), ".exe"), ".msi")
	if name == "" {
		name = "application"
	}
	// The package extension is authoritative. The form's default select value
	// must not cause a user-uploaded EXE to be published as an MSI.
	if strings.HasSuffix(strings.ToLower(source), ".msi") {
		app.InstallerType = "msi"
	} else if strings.HasSuffix(strings.ToLower(source), ".exe") {
		app.InstallerType = "exe"
	}
	if app.Version == "" {
		if m := packageVersionRE.FindString(name); m != "" {
			app.Version = m
		} else {
			app.Version = "0.0.0"
		}
	}
	base := strings.Trim(packageVersionRE.ReplaceAllString(name, ""), " .-_()[]")
	if app.AppID == "" {
		app.AppID = strings.ToLower(strings.Join(strings.FieldsFunc(base, func(r rune) bool { return r == ' ' || r == '_' || r == '-' }), "-"))
		if app.AppID == "" {
			app.AppID = "application"
		}
	}
	if app.DisplayName == "" {
		app.DisplayName = strings.TrimSpace(strings.ReplaceAll(base, "_", " "))
	}
	if app.Detection.Path == "" {
		app.Detection = model.ApplicationDetection{Type: "file_exists", Path: `C:\Program Files\` + app.DisplayName + `\` + app.AppID + `.exe`}
	}
	if model.IsVSCodeApplication(*app) {
		app.InstallerType = "exe"
		app.Publisher = "Microsoft"
		app.DisplayName = "Visual Studio Code"
		app.SilentArgs = model.VSCodeSilentArgs()
		app.AppLockerAllowPath = model.VSCodeAppLockerPath()
		app.Detection = model.ApplicationDetection{Type: "file_exists", Path: `C:\Program Files\Microsoft VS Code\Code.exe`}
		app.Shortcut = model.ApplicationShortcut{Enabled: true, PublicDesktop: true, Name: "Visual Studio Code", Target: `C:\Program Files\Microsoft VS Code\Code.exe`}
	} else if model.IsWeChatApplication(*app) {
		app.InstallerType = "exe"
		app.Publisher = "Tencent"
		app.DisplayName = "WeChat"
		app.SilentArgs = model.WeChatSilentArgs()
		app.AppLockerAllowPath = model.WeChatAppLockerPath()
		app.Detection = model.ApplicationDetection{Type: "file_exists", Path: `C:\Program Files\Tencent\WeChat\WeChat.exe`}
		app.Shortcut = model.ApplicationShortcut{Enabled: true, PublicDesktop: true, Name: "WeChat", Target: `C:\Program Files\Tencent\WeChat\WeChat.exe`}
	}
}

func (s *Server) appManager() *admincore.Manager {
	return &admincore.Manager{Store: s.dbm.objects, Events: s.events}
}

// ensureApplicationAppLocker keeps the security policy and the application
// catalog in step. Existing vendor templates without a manifest path retain
// their known rule. Generic packages use only the reviewed manifest path;
// Program Files installs are covered by the image's base allow rule.
func ensureApplicationAppLocker(ctx context.Context, be backend, app model.Application) error {
	allowPath := app.AppLockerAllowPath
	if allowPath == "" && model.IsTrustedExeApplication(app) {
		allowPath = model.TrustedExeAppLockerPath(app)
	}
	if allowPath == "" {
		return nil
	}
	if err := model.ValidateAppLockerPath(allowPath); err != nil {
		return fmt.Errorf("AppLocker allow path: %w", err)
	}
	return be.MutateAppLockerAllowPaths(ctx, []string{allowPath}, nil)
}

func (s *Server) actionApplicationPublish(sess *session, r *http.Request) error {
	app := model.Application{AppID: formValue(r, "appId"), DisplayName: formValue(r, "displayName"), Publisher: formValue(r, "publisher"), Version: formValue(r, "version"), InstallerType: strings.ToLower(formValue(r, "installerType")), SilentArgs: strings.Fields(formValue(r, "silentArgs")), AppLockerAllowPath: formValue(r, "appLockerAllowPath"), Detection: model.ApplicationDetection{Type: formValue(r, "detectionType"), Path: formValue(r, "detectionPath"), Version: formValue(r, "detectionVersion")}, Shortcut: model.ApplicationShortcut{Enabled: formValue(r, "shortcutEnabled") == "1", PublicDesktop: true, Name: formValue(r, "shortcutName"), Target: formValue(r, "shortcutTarget"), WorkingDirectory: formValue(r, "shortcutWorkingDirectory"), Icon: formValue(r, "shortcutIcon")}}
	inferApplication(r, &app)
	if c := strings.TrimSpace(formValue(r, "confirm")); c != "INSTALL" && c != app.AppID+"@"+app.Version {
		return fmt.Errorf("type INSTALL to confirm")
	}
	fetch, err := s.payloadSource(r)
	if err != nil {
		return err
	}
	client := s.clientKey(r)
	return s.jobs.start("application", app.AppID+"@"+app.Version, func(setStep func(string), setProgress func(done, total int64)) error {
		setStep("获取安装包")
		data, err := fetch(setProgress)
		if err != nil {
			return err
		}
		setStep("上传并审核应用")
		sum, err := s.appManager().PublishApplication(app, data, setProgress)
		if err != nil {
			return err
		}
		if err := ensureApplicationAppLocker(context.Background(), sess.be, app); err != nil {
			return fmt.Errorf("publish application AppLocker rule: %w", err)
		}
		logAudit(client, "published application %s %s (%d bytes, sha256 %s)", app.AppID, app.Version, len(data), sum)
		return nil
	})
}

func (s *Server) actionApplicationInstall(sess *session, r *http.Request) error {
	machine, appID, version := formValue(r, "machine"), formValue(r, "appId"), formValue(r, "version")
	if key := formValue(r, "appKey"); key != "" {
		parts := strings.SplitN(key, "|", 2)
		if len(parts) == 2 {
			appID, version = parts[0], parts[1]
		}
	}
	if machine == "" || appID == "" || version == "" {
		return fmt.Errorf("machine, application and version are required")
	}
	device, err := s.dbm.store.Devices().ByHostname(r.Context(), machine)
	if err != nil {
		return fmt.Errorf("unknown machine %q", machine)
	}
	app, err := s.appManager().GetApplication(appID, version)
	if err != nil {
		return err
	}
	if err := ensureApplicationAppLocker(r.Context(), sess.be, app); err != nil {
		return fmt.Errorf("prepare application AppLocker rule: %w", err)
	}
	if open, err := s.dbm.store.ApplicationTasks().HasOpenForDevice(r.Context(), device.ID, appID); err != nil {
		return err
	} else if open && formValue(r, "replace") != "1" {
		return fmt.Errorf("这台机器已有同一应用的未完成任务；如需替换，请重新确认 replace=1")
	}
	// A policy update normally wakes the Agent, but ask this target to sync
	// explicitly so the rule is present before the install task reaches its
	// post-install AppLocker check.
	if app.AppLockerAllowPath != "" || model.IsTrustedExeApplication(app) {
		if err := sess.be.RequestSync(r.Context(), machine); err != nil {
			return fmt.Errorf("request AppLocker policy sync: %w", err)
		}
	}
	item, err := s.dbm.store.ApplicationTasks().Create(r.Context(), device.ID, appID, version, s.clientKey(r), formValue(r, "allowDowngrade") == "1")
	if err != nil {
		return err
	}
	logAudit(s.clientKey(r), "scheduled application %s %s on %s as %s", appID, version, machine, item.ID)
	return nil
}

func (s *Server) actionApplicationCancel(sess *session, r *http.Request) error {
	id := formValue(r, "taskId")
	if id == "" {
		return fmt.Errorf("task id is required")
	}
	task, err := s.dbm.store.ApplicationTasks().ByID(r.Context(), id)
	if err != nil {
		return err
	}
	if _, err := s.dbm.store.ApplicationTasks().Cancel(r.Context(), id, task.DeviceID); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "cancelled application task %s", id)
	return nil
}

// Deleting a catalog entry removes only its OSS manifest and installer; it
// does not uninstall software from a machine or erase completed task history.
func (s *Server) actionApplicationDelete(sess *session, r *http.Request) error {
	appID, version := strings.TrimSpace(formValue(r, "appId")), strings.TrimSpace(formValue(r, "version"))
	if appID == "" || version == "" {
		return fmt.Errorf("application ID and version are required")
	}
	if err := confirmMatches(r, "confirm", appID+"@"+version); err != nil {
		return err
	}
	manifestKey := ossclient.ApplicationKey(appID, version)
	data, _, err := s.dbm.objects.Get(manifestKey)
	if err != nil {
		return fmt.Errorf("read application manifest: %w", err)
	}
	var app model.Application
	if err := json.Unmarshal(data, &app); err != nil {
		return fmt.Errorf("parse application manifest: %w", err)
	}
	if app.AppID != appID || app.Version != version || app.ObjectKey != ossclient.ApplicationPackageKey(appID, version, app.InstallerType) {
		return fmt.Errorf("application manifest does not match the requested package")
	}
	open, err := s.dbm.store.ApplicationTasks().HasOpen(r.Context(), appID, version)
	if err != nil {
		return err
	}
	if open {
		return fmt.Errorf("application %s@%s still has an active installation task; cancel it first", appID, version)
	}
	// Remove the package first. If the second delete fails, the manifest stays
	// visible so an administrator can retry instead of leaving hidden bytes.
	if err := s.dbm.objects.Delete(app.ObjectKey); err != nil {
		return fmt.Errorf("delete application package: %w", err)
	}
	if err := s.dbm.objects.Delete(manifestKey); err != nil {
		return fmt.Errorf("delete application manifest: %w", err)
	}
	logAudit(s.clientKey(r), "deleted application %s@%s OSS manifest and package; task history retained", appID, version)
	return nil
}
