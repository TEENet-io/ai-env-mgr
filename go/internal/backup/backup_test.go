package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

type fakeStore struct {
	objects map[string]time.Time
	files   map[string]string
	deleted []string
}

func (f *fakeStore) PutFile(key, path string, _ func(int64, int64)) error {
	f.files[key] = path
	f.objects[key] = time.Now()
	return nil
}
func (f *fakeStore) ListInfo(prefix string) ([]ossclient.ObjectInfo, error) {
	var out []ossclient.ObjectInfo
	for k, t := range f.objects {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, ossclient.ObjectInfo{Key: k, LastModified: t})
		}
	}
	return out, nil
}
func (f *fakeStore) Delete(key string) error {
	delete(f.objects, key)
	f.deleted = append(f.deleted, key)
	return nil
}

func TestUploadKeepsThirtyDaysOfDumps(t *testing.T) {
	now := time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC)
	store := &fakeStore{objects: map[string]time.Time{
		Prefix + "aienv-20260821.sql.gz": now.AddDate(0, 0, -32),
		Prefix + "aienv-20260901.sql.gz": now.AddDate(0, 0, -21),
		Prefix + "notes.txt":             now.AddDate(0, 0, -90),
	}, files: map[string]string{}}
	file := filepath.Join(t.TempDir(), "dump.sql.gz")
	os.WriteFile(file, []byte("x"), 0o600)
	key, removed, err := Upload(store, file, now, 30*24*time.Hour)
	if err != nil || key != Prefix+"aienv-20260922.sql.gz" {
		t.Fatalf("upload: %s %v", key, err)
	}
	if store.files[key] != file {
		t.Fatal("the file was not uploaded under today's key")
	}
	if len(removed) != 1 || removed[0] != Prefix+"aienv-20260821.sql.gz" {
		t.Fatalf("removed = %v (only the dump older than 30 days, never the note)", removed)
	}
	if _, ok := store.objects[Prefix+"aienv-20260901.sql.gz"]; !ok {
		t.Fatal("a 21-day-old dump must stay")
	}
}
