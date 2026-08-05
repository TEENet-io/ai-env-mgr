package admincore

import (
	"fmt"
	"sort"
	"time"

	"github.com/TEENet-io/airlock/internal/ossclient"
)

// DefaultLinkTTL is how long a download link stays valid unless the caller
// says otherwise. Long enough to walk over to a machine and paste it, short
// enough that a link left in a chat log stops working.
const DefaultLinkTTL = 6 * time.Hour

// MaxLinkTTL caps how long a link can be made to live. OSS itself allows
// much longer, but a signed URL is a bearer token: anyone who has it can
// fetch the object, and agent.exe contains a credential.
const MaxLinkTTL = 7 * 24 * time.Hour

// StagedFile is one file waiting to be fetched by a machine.
type StagedFile struct {
	Name string
	Key  string
}

// PutFile stages a file for download and returns a link to fetch it with.
//
// This exists because getting agent.exe onto a cloud desktop is otherwise
// awkward: the desktop has no OSS credentials, and console file transfer is
// slow and manual. A signed link needs nothing on the receiving end.
func (m *Manager) PutFile(name string, data []byte, ttl time.Duration) (string, error) {
	if name == "" {
		return "", fmt.Errorf("file name is required")
	}
	key := ossclient.FileKey(name)
	if err := m.Store.Put(key, data); err != nil {
		return "", fmt.Errorf("upload %q: %w", name, err)
	}
	return m.LinkFile(name, ttl)
}

// LinkFile issues a fresh download link for an already-staged file, so a
// link that has expired can be reissued without uploading again.
func (m *Manager) LinkFile(name string, ttl time.Duration) (string, error) {
	ttl = clampTTL(ttl)
	url, err := m.Store.SignedURL(ossclient.FileKey(name), ttl)
	if err != nil {
		return "", err
	}
	return url, nil
}

// ListFiles reports what is currently staged.
func (m *Manager) ListFiles() ([]StagedFile, error) {
	keys, err := m.Store.List(ossclient.FilePrefix)
	if err != nil {
		return nil, fmt.Errorf("list staged files: %w", err)
	}
	files := make([]StagedFile, 0, len(keys))
	for _, k := range keys {
		name := ossclient.FileFromKey(k)
		if name == "" {
			continue // the prefix itself, if the store reports it
		}
		files = append(files, StagedFile{Name: name, Key: k})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// RemoveFile deletes a staged file.
func (m *Manager) RemoveFile(name string) error {
	if err := m.Store.Delete(ossclient.FileKey(name)); err != nil {
		return fmt.Errorf("remove %q: %w", name, err)
	}
	return nil
}

// clampTTL keeps a requested lifetime inside the safe range, treating an
// unset value as the default rather than as zero.
func clampTTL(ttl time.Duration) time.Duration {
	switch {
	case ttl <= 0:
		return DefaultLinkTTL
	case ttl > MaxLinkTTL:
		return MaxLinkTTL
	default:
		return ttl
	}
}
