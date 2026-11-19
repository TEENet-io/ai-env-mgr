package adminweb

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/deviceapi"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/worker"
)

// The Codex installer is the same for every deployment, so it is uploaded
// once, to one bucket, and every console that names that bucket
// (AIENVMGR_CODEX_BUCKET) scans and hands out Codex from there. Everything
// else -- agents, which carry their console's address and so differ per
// deployment, credentials, bindings -- stays in the deployment's own bucket.
//
// The console's key needs only to read that bucket's _codex/ prefix.

// codexSource is what is read from the Codex bucket: the scan's listing and
// checksum, and the download links agents follow.
type codexSource interface {
	packageStore
	deviceapi.Presigner
	deviceapi.DataPresigner
}

// packageStore is the version library as the console reads it: list a
// prefix, check one key, checksum one object.
type packageStore interface {
	worker.PackageSource
	Head(key string) (etag string, exists bool, err error)
}

func isCodexKey(key string) bool { return strings.HasPrefix(key, ossclient.CodexPrefix) }

// packageRouter sends the Codex prefix to the Codex bucket and every other
// key to the deployment's own bucket.
type packageRouter struct {
	main  packageStore
	codex packageStore
}

func (p packageRouter) Head(key string) (string, bool, error) {
	if isCodexKey(key) {
		return p.codex.Head(key)
	}
	return p.main.Head(key)
}

func (p packageRouter) ListInfo(prefix string) ([]ossclient.ObjectInfo, error) {
	if isCodexKey(prefix) {
		return p.codex.ListInfo(prefix)
	}
	return p.main.ListInfo(prefix)
}

func (p packageRouter) HashObject(key string) (string, int64, error) {
	if isCodexKey(key) {
		return p.codex.HashObject(key)
	}
	return p.main.HashObject(key)
}

// signerRouter is the same split for the links an agent is given. Uploads
// never go to the Codex bucket: agents do not write there.
type signerRouter struct {
	main  deviceapi.Presigner
	codex codexSource
}

func (s signerRouter) SignedURL(key string, ttl time.Duration) (string, error) {
	if isCodexKey(key) {
		return s.codex.SignedURL(key, ttl)
	}
	return s.main.SignedURL(key, ttl)
}

func (s signerRouter) SignedPutURL(key string, ttl time.Duration, contentType string) (string, error) {
	return s.main.SignedPutURL(key, ttl, contentType)
}

func (s signerRouter) SignedDataURL(key string, ttl time.Duration) (string, error) {
	if isCodexKey(key) {
		return s.codex.SignedDataURL(key, ttl)
	}
	if d, ok := s.main.(deviceapi.DataPresigner); ok {
		return d.SignedDataURL(key, ttl)
	}
	return s.main.SignedURL(key, ttl)
}

func (s signerRouter) SignedDataPutURL(key string, ttl time.Duration, contentType string) (string, error) {
	if d, ok := s.main.(deviceapi.DataPresigner); ok {
		return d.SignedDataPutURL(key, ttl, contentType)
	}
	return s.main.SignedPutURL(key, ttl, contentType)
}

// useCodexBucket points the version library's Codex prefix, and the Codex
// download links, at the shared Codex bucket. The bucket is checked once at
// start: a key that cannot list _codex/ there is a misconfiguration worth
// refusing to start over, not one to discover when a machine needs Codex.
func (s *Server) useCodexBucket(st *dbState, opts DatabaseOptions) error {
	dialed, err := s.dialOSS(config.Config{
		Endpoint: s.opts.Endpoint, Bucket: opts.CodexBucket,
		AccessKeyID: opts.OSSAccessKeyID, AccessKeySecret: opts.OSSAccessKeySecret,
	})
	if err != nil {
		return fmt.Errorf("codex bucket %s: %w", opts.CodexBucket, err)
	}
	codex, ok := dialed.(codexSource)
	if !ok {
		return fmt.Errorf("codex bucket %s: the store cannot list, checksum and sign", opts.CodexBucket)
	}
	if _, err := codex.ListInfo(ossclient.CodexPrefix); err != nil {
		return fmt.Errorf("codex bucket %s: cannot list %s (the server key needs read there): %w", opts.CodexBucket, ossclient.CodexPrefix, err)
	}
	if st.packages == nil || st.devices.Objects == nil {
		return errors.New("codex bucket: this deployment's own store cannot serve the version library")
	}
	st.packages = packageRouter{main: st.packages, codex: codex}
	st.devices.Objects = signerRouter{main: st.devices.Objects, codex: codex}
	return nil
}
