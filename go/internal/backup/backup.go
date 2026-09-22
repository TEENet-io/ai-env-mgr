// Package backup puts a database dump in the bucket and keeps only the
// recent ones. The dump itself is made outside (pg_dump on the host); this
// is the part that needs the bucket credentials the console already holds.
package backup

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// Prefix is where dumps live in the bucket.
const Prefix = ossclient.Root + "_backup/"

// Store is the slice of the bucket the backup uses.
type Store interface {
	PutFile(key, path string, onProgress func(done, total int64)) error
	ListInfo(prefix string) ([]ossclient.ObjectInfo, error)
	Delete(key string) error
}

// Key names today's dump.
func Key(now time.Time) string {
	return Prefix + "aienv-" + now.UTC().Format("20060102") + ".sql.gz"
}

// Upload stores the file as today's dump and removes dumps older than keep.
// It returns the key written and the keys removed. Pruning happens only
// after a successful upload: a night the upload failed must not also be
// the night an old copy went.
func Upload(store Store, file string, now time.Time, keep time.Duration) (string, []string, error) {
	key := Key(now)
	if err := store.PutFile(key, file, nil); err != nil {
		return "", nil, fmt.Errorf("upload %s: %w", key, err)
	}
	objects, err := store.ListInfo(Prefix)
	if err != nil {
		return key, nil, fmt.Errorf("list backups: %w", err)
	}
	cutoff := now.Add(-keep)
	var removed []string
	for _, o := range objects {
		if o.Key == key || !strings.HasSuffix(o.Key, ".sql.gz") || !o.LastModified.Before(cutoff) {
			continue
		}
		if err := store.Delete(o.Key); err != nil {
			return key, removed, fmt.Errorf("remove %s: %w", path.Base(o.Key), err)
		}
		removed = append(removed, o.Key)
	}
	sort.Strings(removed)
	return key, removed, nil
}
