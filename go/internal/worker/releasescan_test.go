package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func (f *fakeObjects) ListInfo(prefix string) ([]ossclient.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ossclient.ObjectInfo
	for key, data := range f.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, ossclient.ObjectInfo{Key: key, Size: int64(len(data))})
		}
	}
	return out, nil
}

func (f *fakeObjects) HashObject(key string) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashes++
	data, ok := f.objects[key]
	if !ok {
		return "", 0, ossclient.ErrNotFound
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

func TestTheScanRegistersWhatCIUploadedAndAimsAtNobody(t *testing.T) {
	store, ctx := newWorkerStore(t)
	objects := newFakeObjects()
	objects.objects[ossclient.CodexInstallerKey("0.42.0")] = []byte("codex installer")
	objects.objects[ossclient.AgentVersionKey("1.2.16")] = []byte("agent binary")
	objects.objects[ossclient.AgentBinaryKey()] = []byte("the fixed key is not a version")
	objects.objects[ossclient.CodexPrefix+"README.txt"] = []byte("not a package")
	svc := ops.New(store)
	h := &ReleaseScan{Store: store, Objects: objects, Ops: svc}

	res, err := h.Run(ctx, repo.Task{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !strings.HasPrefix(res.Note, "registered 2, refused 0") {
		t.Fatalf("note = %q", res.Note)
	}
	codex, err := store.Releases().ArtifactByVersion(ctx, repo.ProductCodex, "0.42.0")
	if err != nil {
		t.Fatalf("codex not registered: %v", err)
	}
	sum := sha256.Sum256([]byte("codex installer"))
	if codex.SHA256 != hex.EncodeToString(sum[:]) || codex.Status != repo.ArtifactCandidate || codex.CreatedBy != "ci" {
		t.Fatalf("codex artifact = %+v", codex)
	}
	if _, err := store.Releases().ArtifactByVersion(ctx, repo.ProductAgent, "1.2.16"); err != nil {
		t.Fatalf("agent not registered: %v", err)
	}
	if all, _ := store.Releases().ListArtifacts(ctx, ""); len(all) != 2 {
		t.Fatalf("%d artifacts; the fixed key and the stray file must not be registered", len(all))
	}
	pol, _, _ := svc.CurrentPolicy(ctx)
	if pol.CodexVersion != "" || pol.AgentUpdateVersion != "" {
		t.Fatal("a scan must not aim the fleet at anything")
	}

	// A second pass reads nothing again: the two are registered.
	hashed := objects.hashes
	if _, err := h.Run(ctx, repo.Task{}); err != nil || objects.hashes != hashed {
		t.Fatalf("second pass hashed %d more object(s), err %v", objects.hashes-hashed, err)
	}
}

func TestTheScanDoesNotRehashWhatItCouldNotRegister(t *testing.T) {
	store, ctx := newWorkerStore(t)
	objects := newFakeObjects()
	svc := ops.New(store)
	// The version is already taken by different bytes.
	if _, err := svc.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.42.0",
		SHA256: strings.Repeat("a", 64), SizeBytes: 1, ObjectKey: "elsewhere", CreatedBy: "t"}, "t", "r"); err != nil {
		t.Fatal(err)
	}
	// ...and an object of that version turns up under the library key, but
	// ArtifactByVersion already answers for it, so it is skipped without a
	// read. Make the conflict real by registering a different version's
	// bytes under a key the scan will try.
	objects.objects[ossclient.CodexInstallerKey("0.43.0")] = []byte("new bytes")
	svc.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.43.0",
		SHA256: strings.Repeat("b", 64), SizeBytes: 1, ObjectKey: "elsewhere-2", CreatedBy: "t"}, "t", "r2")
	h := &ReleaseScan{Store: store, Objects: objects, Ops: svc}
	if _, err := h.Run(ctx, repo.Task{}); err != nil {
		t.Fatal(err)
	}
	if objects.hashes != 0 {
		t.Fatalf("a version the library already names must not be read, hashed %d", objects.hashes)
	}
}
