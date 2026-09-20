package worker

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// PackageSource is the part of the bucket the scan reads: the version
// library's prefixes, and one object's checksum.
type PackageSource interface {
	ListInfo(prefix string) ([]ossclient.ObjectInfo, error)
	HashObject(key string) (sha256hex string, size int64, err error)
}

// ReleaseScan registers what CI put in the bucket. Every few minutes it lists
// the version library's prefixes; an object at a versioned key that no
// artifact names yet is read once, checksummed, and registered as a
// candidate. That is all: a candidate aims at nobody. The administrator sees
// it on the releases page, accepts it on the test machine, and hands it out.
type ReleaseScan struct {
	Store   repo.Store
	Objects PackageSource
	Ops     *ops.Service

	// A key whose registration failed -- most often bytes that differ from
	// what the version already names -- is not hashed again every pass: a
	// 700 MB read per five minutes, forever, for a mistake only a person can
	// resolve. It is retried after a restart.
	mu      sync.Mutex
	refused map[string]string
}

// Run scans both products.
func (h *ReleaseScan) Run(ctx context.Context, _ repo.Task) (Result, error) {
	registered, refused := 0, 0
	var notes []string
	for _, product := range []string{repo.ProductCodex, repo.ProductAgent} {
		prefix := ossclient.CodexPrefix
		if product == repo.ProductAgent {
			prefix = ossclient.AgentPrefix
		}
		objects, err := h.Objects.ListInfo(prefix)
		if err != nil {
			return Result{}, ClassError("oss_list", err)
		}
		for _, o := range objects {
			version := versionFromKey(product, o.Key)
			if version == "" || h.isRefused(o.Key) {
				continue
			}
			if _, err := h.Store.Releases().ArtifactByVersion(ctx, product, version); err == nil {
				continue
			}
			sum, size, err := h.Objects.HashObject(o.Key)
			if err != nil {
				return Result{Note: strings.Join(notes, "; ")}, ClassError("oss_read", err)
			}
			_, err = h.Ops.RegisterArtifact(ctx, repo.NewArtifact{
				Product: product, Version: version, SHA256: sum, SizeBytes: size,
				ObjectKey: o.Key, Source: "oss:" + o.Key, CreatedBy: "ci",
			}, "ci", "release_scan:"+o.Key)
			if err != nil {
				h.refuse(o.Key, err.Error())
				refused++
				notes = append(notes, fmt.Sprintf("%s: %v", o.Key, err))
				continue
			}
			registered++
			notes = append(notes, fmt.Sprintf("registered %s %s", product, version))
		}
	}
	note := fmt.Sprintf("registered %d, refused %d", registered, refused)
	if len(notes) > 0 {
		note += ": " + strings.Join(notes, "; ")
	}
	return Result{Note: note}, nil
}

func versionFromKey(product, key string) string {
	if product == repo.ProductCodex {
		return ossclient.CodexVersionFromKey(key)
	}
	return ossclient.AgentVersionFromKey(key)
}

func (h *ReleaseScan) isRefused(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.refused[key]
	return ok
}

func (h *ReleaseScan) refuse(key, why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refused == nil {
		h.refused = map[string]string{}
	}
	h.refused[key] = why
}
