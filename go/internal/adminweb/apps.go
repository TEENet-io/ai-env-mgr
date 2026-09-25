package adminweb

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
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
	if _, err := s.dbm.store.Devices().ByHostname(r.Context(), machine); err != nil {
		return fmt.Errorf("unknown machine %q", machine)
	}
	item, err := s.appManager().InstallApplication(machine, appID, version, formValue(r, "allowDowngrade") == "1")
	if err != nil {
		return err
	}
	logAudit(s.clientKey(r), "scheduled application %s %s on %s as %s", appID, version, machine, item.TaskID)
	return nil
}

func (s *Server) actionApplicationCancel(sess *session, r *http.Request) error {
	machine, appID := formValue(r, "machine"), formValue(r, "appId")
	if machine == "" || appID == "" {
		return fmt.Errorf("machine and application are required")
	}
	if err := s.appManager().CancelApplication(machine, appID); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "cancelled application %s on %s", appID, machine)
	return nil
}
