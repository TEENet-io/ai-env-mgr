package admincore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func testApplication() model.Application {
	return model.Application{
		AppID: "editor", DisplayName: "Editor", Publisher: "Acme", Version: "1.0.0",
		InstallerType: "msi", Detection: model.ApplicationDetection{Type: "file_exists", Path: `C:\Program Files\Editor\editor.exe`},
		Shortcut: model.ApplicationShortcut{Enabled: true, PublicDesktop: true, Name: "Editor", Target: `C:\Program Files\Editor\editor.exe`},
	}
}

func TestPublishAndScheduleApplication(t *testing.T) {
	store := newFakeStore()
	m := &Manager{Store: store}
	app := testApplication()
	bin := []byte("installer")
	sum := sha256.Sum256(bin)
	got, err := m.PublishApplication(app, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha = %q, want %x", got, sum)
	}
	if _, ok := store.objects[ossclient.ApplicationKey(app.AppID, app.Version)]; !ok {
		t.Fatal("manifest was not written")
	}
	item, err := m.InstallApplication("PC-1", app.AppID, app.Version, false)
	if err != nil {
		t.Fatal(err)
	}
	if item.TaskID == "" || item.Desired != "installed" {
		t.Fatalf("bad desired item: %+v", item)
	}
	var desired model.MachineApplications
	if err := json.Unmarshal(store.objects[ossclient.MachineApplicationsKey("PC-1")], &desired); err != nil {
		t.Fatal(err)
	}
	if len(desired.Apps) != 1 || desired.Apps[0].AppID != app.AppID {
		t.Fatalf("desired state = %+v", desired)
	}
}

func TestInstallApplicationRejectsUnapprovedManifest(t *testing.T) {
	store := newFakeStore()
	app := testApplication()
	app.ObjectKey = ossclient.ApplicationPackageKey(app.AppID, app.Version, app.InstallerType)
	app.Enabled = true
	app.Approved = false
	b, _ := json.Marshal(app)
	store.objects[ossclient.ApplicationKey(app.AppID, app.Version)] = b
	m := &Manager{Store: store}
	if _, err := m.InstallApplication("PC-1", app.AppID, app.Version, false); err == nil {
		t.Fatal("unapproved application should not be scheduled")
	}
}

func TestCancelApplicationMarksDesiredStateAbsent(t *testing.T) {
	store := newFakeStore()
	m := &Manager{Store: store}
	app := testApplication()
	if _, err := m.PublishApplication(app, []byte("installer"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.InstallApplication("PC-1", app.AppID, app.Version, false); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelApplication("PC-1", app.AppID); err != nil {
		t.Fatal(err)
	}
	var desired model.MachineApplications
	if err := json.Unmarshal(store.objects[ossclient.MachineApplicationsKey("PC-1")], &desired); err != nil {
		t.Fatal(err)
	}
	if desired.Apps[0].Desired != "absent" {
		t.Fatalf("desired state = %+v", desired.Apps[0])
	}
}
