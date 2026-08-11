package admincore

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func TestFetchLogReturnsUploadedLog(t *testing.T) {
	fs := newFakeStore()
	m := &Manager{Store: fs}
	fs.objects[ossclient.LogKey("DESKTOP-A")] = []byte("startup sync ok\nscheduled sync ok\n")

	got, err := m.FetchLog("DESKTOP-A")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "startup sync ok\nscheduled sync ok\n" {
		t.Fatalf("unexpected log: %q", got)
	}
}

func TestFetchLogMissingIsAnError(t *testing.T) {
	m := &Manager{Store: newFakeStore()}
	if _, err := m.FetchLog("never-uploaded"); err == nil {
		t.Fatal("expected an error when no log has been uploaded")
	}
}
