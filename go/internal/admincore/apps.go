package admincore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// PublishApplication stores an approved installer and its immutable manifest.
// The manifest is written last, so an agent never sees a catalog entry before
// its package is present.
func (m *Manager) PublishApplication(app model.Application, installer []byte, onProgress func(done, total int64)) (string, error) {
	if err := validateApplication(app, installer); err != nil {
		return "", err
	}
	sum := sha256.Sum256(installer)
	app.SHA256 = hex.EncodeToString(sum[:])
	app.Size = int64(len(installer))
	app.ObjectKey = ossclient.ApplicationPackageKey(app.AppID, app.Version, app.InstallerType)
	app.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	app.Enabled = true
	app.Approved = true

	if err := m.Store.Put(app.ObjectKey, installer); err != nil {
		return "", fmt.Errorf("upload application installer: %w", err)
	}
	manifest, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode application manifest: %w", err)
	}
	if err := m.Store.Put(ossclient.ApplicationKey(app.AppID, app.Version), manifest); err != nil {
		return "", fmt.Errorf("publish application manifest: %w", err)
	}
	return app.SHA256, nil
}

func validateApplication(app model.Application, installer []byte) error {
	if strings.TrimSpace(app.AppID) == "" || strings.TrimSpace(app.Version) == "" {
		return fmt.Errorf("application id and version are required")
	}
	if len(installer) == 0 {
		return fmt.Errorf("the application installer is empty")
	}
	if !strings.EqualFold(app.InstallerType, "msi") && !(strings.EqualFold(app.InstallerType, "exe") && model.IsVSCodeApplication(app)) {
		return fmt.Errorf("only MSI or the trusted VS Code installer is supported")
	}
	if app.Detection.Path == "" || (app.Detection.Type != "file_exists" && app.Detection.Type != "file_version") {
		return fmt.Errorf("a file_exists or file_version detection rule is required")
	}
	if app.Shortcut.Enabled && (app.Shortcut.Name == "" || app.Shortcut.Target == "") {
		return fmt.Errorf("shortcut name and target are required when shortcuts are enabled")
	}
	return nil
}

// ListApplications lists all catalog manifests. Disabled entries are kept so
// administrators can see why an older task can no longer be scheduled.
func (m *Manager) ListApplications() ([]model.Application, error) {
	keys, err := m.Store.List(ossclient.AppPrefix)
	if err != nil {
		return nil, fmt.Errorf("list application catalog: %w", err)
	}
	var apps []model.Application
	for _, key := range keys {
		if !strings.HasSuffix(key, "/manifest.json") {
			continue
		}
		data, _, err := m.Store.Get(key)
		if err != nil {
			return nil, fmt.Errorf("read application manifest %q: %w", key, err)
		}
		var app model.Application
		if err := json.Unmarshal(data, &app); err != nil {
			return nil, fmt.Errorf("parse application manifest %q: %w", key, err)
		}
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].AppID == apps[j].AppID {
			return apps[i].Version < apps[j].Version
		}
		return apps[i].AppID < apps[j].AppID
	})
	return apps, nil
}

func (m *Manager) GetApplication(appID, version string) (model.Application, error) {
	data, _, err := m.Store.Get(ossclient.ApplicationKey(appID, version))
	if err != nil {
		return model.Application{}, fmt.Errorf("read application: %w", err)
	}
	var app model.Application
	if err := json.Unmarshal(data, &app); err != nil {
		return model.Application{}, fmt.Errorf("parse application: %w", err)
	}
	if !app.Enabled || !app.Approved {
		return model.Application{}, fmt.Errorf("application %q version %q is not enabled and approved", appID, version)
	}
	if !strings.EqualFold(app.InstallerType, "msi") && !(strings.EqualFold(app.InstallerType, "exe") && model.IsVSCodeApplication(app)) {
		return model.Application{}, fmt.Errorf("application %q version %q is not supported: only MSI or the trusted VS Code installer is allowed", appID, version)
	}
	if app.ObjectKey != ossclient.ApplicationPackageKey(app.AppID, app.Version, app.InstallerType) {
		return model.Application{}, fmt.Errorf("application manifest points outside its approved package key")
	}
	return app, nil
}

// InstallApplication appends/replaces one desired machine state. It checks
// the catalog first, so a task can never reference an unapproved package.
func (m *Manager) InstallApplication(machine, appID, version string, allowDowngrade bool) (model.DesiredApplication, error) {
	if strings.TrimSpace(machine) == "" {
		return model.DesiredApplication{}, fmt.Errorf("machine is required")
	}
	app, err := m.GetApplication(appID, version)
	if err != nil {
		return model.DesiredApplication{}, err
	}
	data, _, err := m.Store.Get(ossclient.MachineApplicationsKey(machine))
	if err != nil && !isNotFound(err) {
		return model.DesiredApplication{}, fmt.Errorf("read machine applications: %w", err)
	}
	desired := model.MachineApplications{Machine: machine}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &desired); err != nil {
			return model.DesiredApplication{}, fmt.Errorf("parse machine applications: %w", err)
		}
	}
	item := model.DesiredApplication{
		AppID: app.AppID, Version: app.Version, Desired: "installed",
		TaskID: fmt.Sprintf("install-%d", time.Now().UTC().UnixNano()), AllowDowngrade: allowDowngrade,
	}
	found := false
	for i := range desired.Apps {
		if desired.Apps[i].AppID == item.AppID {
			desired.Apps[i] = item
			found = true
			break
		}
	}
	if !found {
		desired.Apps = append(desired.Apps, item)
	}
	desired.Machine = machine
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	out, err := json.MarshalIndent(desired, "", "  ")
	if err != nil {
		return model.DesiredApplication{}, fmt.Errorf("encode machine applications: %w", err)
	}
	if err := m.Store.Put(ossclient.MachineApplicationsKey(machine), out); err != nil {
		return model.DesiredApplication{}, fmt.Errorf("save machine applications: %w", err)
	}
	return item, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, ossclient.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "not found")
}

func (m *Manager) CancelApplication(machine, appID string) error {
	data, _, err := m.Store.Get(ossclient.MachineApplicationsKey(machine))
	if err != nil {
		return err
	}
	var desired model.MachineApplications
	if err := json.Unmarshal(data, &desired); err != nil {
		return err
	}
	for i := range desired.Apps {
		if desired.Apps[i].AppID == appID {
			desired.Apps[i].Desired = "absent"
		}
	}
	desired.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	out, err := json.MarshalIndent(desired, "", "  ")
	if err != nil {
		return err
	}
	return m.Store.Put(ossclient.MachineApplicationsKey(machine), out)
}
