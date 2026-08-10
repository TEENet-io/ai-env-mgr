// Package creds packs, unpacks and deploys AI tool credentials.
//
// Packing happens entirely in memory: the admin machine assembles a user's
// logins and uploads them without ever writing tokens to its own disk.
package creds

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"sort"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// maxEntrySize bounds how much a single archive entry may expand to. The
// archives are our own and hold small JSON files, so anything larger means a
// corrupt or hostile object rather than a legitimate credential.
const maxEntrySize = 1 << 20 // 1 MiB

// Pack zips a credential set. Entries are written in sorted order so the same
// input always produces the same archive.
func Pack(set model.CredentialSet) ([]byte, error) {
	if len(set) == 0 {
		return nil, fmt.Errorf("credential set is empty")
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			return nil, fmt.Errorf("create zip entry %q: %w", name, err)
		}
		if _, err := w.Write(set[name]); err != nil {
			return nil, fmt.Errorf("write zip entry %q: %w", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close zip: %w", err)
	}
	return buf.Bytes(), nil
}

// Unpack reads a credentials archive back into memory.
func Unpack(blob []byte) (model.CredentialSet, error) {
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	set := model.CredentialSet{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry %q: %w", f.Name, err)
		}
		// Cap the read so a malformed archive cannot exhaust memory.
		data, err := io.ReadAll(io.LimitReader(rc, maxEntrySize+1))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read zip entry %q: %w", f.Name, err)
		}
		if len(data) > maxEntrySize {
			return nil, fmt.Errorf("zip entry %q exceeds %d bytes", f.Name, maxEntrySize)
		}
		set[f.Name] = data
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("archive contains no entries")
	}
	return set, nil
}

// Merge overlays new entries onto existing ones, returning a fresh set.
//
// Publishing only Claude credentials must not drop the Codex ones already
// stored for that user, so the admin merges before uploading.
func Merge(existing, incoming model.CredentialSet) model.CredentialSet {
	out := model.CredentialSet{}
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range incoming {
		out[k] = v
	}
	return out
}
