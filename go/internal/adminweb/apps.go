package adminweb

import (
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
	if app.InstallerType == "" {
		if strings.HasSuffix(strings.ToLower(source), ".msi") {
			app.InstallerType = "msi"
		} else {
			app.InstallerType = "exe"
		}
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
}

func (s *Server) appManager() *admincore.Manager {
	return &admincore.Manager{Store: s.dbm.objects, Events: s.events}
}

func (s *Server) actionApplicationPublish(sess *session, r *http.Request) error {
	app := model.Application{AppID: formValue(r, "appId"), DisplayName: formValue(r, "displayName"), Publisher: formValue(r, "publisher"), Version: formValue(r, "version"), InstallerType: strings.ToLower(formValue(r, "installerType")), SilentArgs: strings.Fields(formValue(r, "silentArgs")), Detection: model.ApplicationDetection{Type: formValue(r, "detectionType"), Path: formValue(r, "detectionPath"), Version: formValue(r, "detectionVersion")}, Shortcut: model.ApplicationShortcut{Enabled: formValue(r, "shortcutEnabled") == "1", PublicDesktop: true, Name: formValue(r, "shortcutName"), Target: formValue(r, "shortcutTarget"), WorkingDirectory: formValue(r, "shortcutWorkingDirectory"), Icon: formValue(r, "shortcutIcon")}}
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
	if _, err := s.appManager().GetApplication(appID, version); err != nil {
		return err
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
