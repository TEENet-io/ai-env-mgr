package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

type fakeApplicationInstaller struct {
	installed string
	installs  int
}

func (f *fakeApplicationInstaller) InstalledVersion(model.Application) (string, error) {
	return f.installed, nil
}
func (f *fakeApplicationInstaller) Running(model.Application) (bool, error)     { return false, nil }
func (f *fakeApplicationInstaller) FreeBytes(model.Application) (uint64, error) { return 10 << 30, nil }
func (f *fakeApplicationInstaller) Install(_ model.Application, _ string) error {
	f.installs++
	return nil
}
func (f *fakeApplicationInstaller) EnsureShortcut(model.Application) (bool, error) { return true, nil }
func (f *fakeApplicationInstaller) LaunchAsStandardUser(model.Application) (bool, error) {
	return true, nil
}
func (f *fakeApplicationInstaller) AppLockerAllowed(model.Application) (bool, error) {
	return true, nil
}

func TestRunOnceInstallsApprovedApplication(t *testing.T) {
	store := newFakeStore()
	s := newSyncer(t, store, &fakeApplier{})
	packageData := []byte("application installer")
	sum := sha256.Sum256(packageData)
	app := model.Application{
		AppID: "editor", Version: "1.0.0", InstallerType: "msi", Enabled: true, Approved: true,
		ObjectKey: ossclient.ApplicationPackageKey("editor", "1.0.0", "msi"), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(packageData)),
		Detection: model.ApplicationDetection{Type: "file_exists", Path: `C:\Program Files\Editor\editor.exe`},
	}
	manifest, _ := json.Marshal(app)
	store.set(ossclient.ApplicationKey(app.AppID, app.Version), manifest, "manifest")
	store.set(app.ObjectKey, packageData, "package")
	desired := model.MachineApplications{Machine: "DESKTOP-A", Apps: []model.DesiredApplication{{AppID: app.AppID, Version: app.Version, Desired: "installed", TaskID: "task-1"}}}
	desiredBytes, _ := json.Marshal(desired)
	store.set(ossclient.MachineApplicationsKey("DESKTOP-A"), desiredBytes, "desired")
	bind(t, store, "DESKTOP-A", "work1")
	fake := &fakeApplicationInstaller{}
	s.Applications = fake

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Apps) != 1 || st.Apps[0].State != AppSucceeded {
		t.Fatalf("application status = %+v, errors=%v", st.Apps, st.Errors)
	}
	if fake.installs != 1 {
		t.Fatalf("install calls = %d, want 1", fake.installs)
	}
}

func TestRunOnceBlocksApplicationChecksumMismatch(t *testing.T) {
	store := newFakeStore()
	s := newSyncer(t, store, &fakeApplier{})
	installer := []byte("real installer")
	app := model.Application{
		AppID: "editor", Version: "1.0.0", InstallerType: "msi", Enabled: true, Approved: true,
		ObjectKey: ossclient.ApplicationPackageKey("editor", "1.0.0", "msi"), SHA256: "wrong", Size: int64(len(installer)),
		Detection: model.ApplicationDetection{Type: "file_exists", Path: `C:\Program Files\Editor\editor.exe`},
	}
	manifest, _ := json.Marshal(app)
	store.set(ossclient.ApplicationKey(app.AppID, app.Version), manifest, "manifest")
	store.set(app.ObjectKey, installer, "package")
	desiredBytes, _ := json.Marshal(model.MachineApplications{Machine: "DESKTOP-A", Apps: []model.DesiredApplication{{AppID: app.AppID, Version: app.Version, Desired: "installed"}}})
	store.set(ossclient.MachineApplicationsKey("DESKTOP-A"), desiredBytes, "desired")
	bind(t, store, "DESKTOP-A", "work1")
	fake := &fakeApplicationInstaller{}
	s.Applications = fake
	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Apps) != 1 || st.Apps[0].State != AppBlocked {
		t.Fatalf("application status = %+v", st.Apps)
	}
	if fake.installs != 0 {
		t.Fatal("checksum mismatch must not run installer")
	}
}
