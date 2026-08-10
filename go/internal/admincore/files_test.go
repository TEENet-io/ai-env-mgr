package admincore

import (
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func TestPutFileStagesAndLinks(t *testing.T) {
	m, store := newManager()

	url, err := m.PutFile("agent.exe", []byte("MZ\x00binary"), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := ossclient.FileKey("agent.exe")
	if _, ok := store.objects[key]; !ok {
		t.Errorf("file should be stored at %s", key)
	}
	if url == "" {
		t.Error("PutFile should return a download link")
	}
	if len(store.signed) != 1 || store.signed[0].Key != key {
		t.Errorf("expected a signature request for %s, got %+v", key, store.signed)
	}
	if store.signed[0].TTL != 2*time.Hour {
		t.Errorf("TTL = %s, want 2h", store.signed[0].TTL)
	}
}

// Staged files must land beside the roster, not inside the agent's
// directory: no agent ever reads them, and the agent's authorisation should
// not have to be widened to cover a transfer area.
func TestStagedFilesAreOutsideTheAgentDirectory(t *testing.T) {
	key := ossclient.FileKey("agent.exe")
	if strings.HasPrefix(key, ossclient.Root) {
		t.Errorf("%q is inside the agent's directory %s", key, ossclient.Root)
	}
	if !strings.HasPrefix(key, ossclient.AdminRoot) {
		t.Errorf("%q should be under %s", key, ossclient.AdminRoot)
	}
}

// A file name arriving from a command line must not be able to write
// anywhere but the staging area.
func TestFileKeyResistsTraversal(t *testing.T) {
	for _, name := range []string{"../users.json", "../../agent_workdir/policy.json", `..\evil`, "a/b/c", ""} {
		key := ossclient.FileKey(name)
		if !strings.HasPrefix(key, ossclient.FilePrefix) {
			t.Errorf("FileKey(%q) = %q escaped the staging area", name, key)
		}
		if strings.Contains(strings.TrimPrefix(key, ossclient.FilePrefix), "/") {
			t.Errorf("FileKey(%q) = %q is more than one level deep", name, key)
		}
	}
}

// A link is a bearer token: anyone holding it can fetch a binary that
// contains a credential. An unbounded lifetime would turn a pasted link into
// a permanent back door.
func TestLinkLifetimeIsClamped(t *testing.T) {
	m, store := newManager()

	if _, err := m.LinkFile("agent.exe", 0); err != nil {
		t.Fatal(err)
	}
	if got := store.signed[0].TTL; got != DefaultLinkTTL {
		t.Errorf("unset TTL = %s, want the default %s", got, DefaultLinkTTL)
	}

	if _, err := m.LinkFile("agent.exe", 365*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := store.signed[1].TTL; got != MaxLinkTTL {
		t.Errorf("an excessive TTL should be capped at %s, got %s", MaxLinkTTL, got)
	}

	if _, err := m.LinkFile("agent.exe", -time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := store.signed[2].TTL; got != DefaultLinkTTL {
		t.Errorf("a negative TTL should fall back to the default, got %s", got)
	}
}

func TestListAndRemoveFiles(t *testing.T) {
	m, store := newManager()
	for _, name := range []string{"agent.exe", "notes.txt"} {
		if _, err := m.PutFile(name, []byte("x"), time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	files, err := m.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "agent.exe" || files[1].Name != "notes.txt" {
		t.Fatalf("ListFiles = %+v, want the two staged names sorted", files)
	}

	if err := m.RemoveFile("notes.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.objects[ossclient.FileKey("notes.txt")]; ok {
		t.Error("RemoveFile should delete the object")
	}
}

func TestPutFileRejectsAnEmptyName(t *testing.T) {
	m, _ := newManager()
	if _, err := m.PutFile("", []byte("x"), time.Hour); err == nil {
		t.Error("an empty name should be rejected rather than writing to the prefix itself")
	}
}
