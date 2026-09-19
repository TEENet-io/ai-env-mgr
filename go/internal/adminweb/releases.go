package adminweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ghrelease"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// maxPackageBytes bounds one package on disk. The Codex installer is ~700 MB;
// two gigabytes leaves room for it to grow without letting a typo fill the
// disk the database shares.
const maxPackageBytes = 2 << 30

// packageUploader is the one method of the object store that moves a large
// file without holding it: the console runs under a memory cap smaller than
// a Codex installer, so Put is not an option for packages.
type packageUploader interface {
	PutFile(key, path string, onProgress func(done, total int64)) error
}

// packageHasher checksums an object already in the bucket without holding
// it, for registering a package CI uploaded directly.
type packageHasher interface {
	Head(key string) (etag string, exists bool, err error)
	HashObject(key string) (sha256hex string, size int64, err error)
}

// artifactKey is where a product's package of a version lives. CI uploads
// to exactly these keys; the console registers whatever it finds there.
func artifactKey(product, version string) string {
	if product == repo.ProductCodex {
		return ossclient.CodexInstallerKey(version)
	}
	return ossclient.AgentVersionKey(version)
}

// stagedPackage is a package on local disk, complete and checksummed.
type stagedPackage struct {
	Path   string
	SHA256 string
	Size   int64
	Source string // the URL or the uploaded file's name, for the record
}

func (p stagedPackage) Remove() { os.Remove(p.Path) }

func spoolName(spool string) string {
	return filepath.Join(spool, "pkg-"+time.Now().UTC().Format("20060102-150405.000")+".bin")
}

// stagePackage validates the request now and returns a closure that puts the
// bytes on disk later, in the background job. A browser upload is read from
// the request body here (it is gone when the handler returns) straight into
// the spool file; a URL is fetched later, straight into the spool file. In
// neither case is the package ever whole in memory.
func stagePackage(r *http.Request, spool string, maxBytes int64) (func(onProgress func(done, total int64)) (stagedPackage, error), error) {
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, fmt.Errorf("spool directory: %w", err)
	}
	if url := formValue(r, "url"); url != "" {
		token := releaseToken(r)
		return func(onProgress func(done, total int64)) (stagedPackage, error) {
			dest := spoolName(spool)
			fetched, err := ghrelease.FetchToFile(url, token, 30*time.Minute, dest, maxBytes, onProgress)
			if err != nil {
				return stagedPackage{}, err
			}
			return stagedPackage{Path: dest, SHA256: fetched.SHA256, Size: fetched.Size, Source: url}, nil
		}, nil
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("give a URL or choose a file")
	}
	defer f.Close()
	if hdr.Size > maxBytes {
		return nil, fmt.Errorf("that file is %d MB; packages are capped at %d MB", hdr.Size>>20, maxBytes>>20)
	}
	dest := spoolName(spool)
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, sum), io.LimitReader(f, maxBytes+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > maxBytes {
		err = fmt.Errorf("the upload is larger than the %d MB limit", maxBytes>>20)
	}
	if err != nil {
		os.Remove(dest)
		return nil, err
	}
	staged := stagedPackage{Path: dest, SHA256: hex.EncodeToString(sum.Sum(nil)), Size: n, Source: hdr.Filename}
	return func(onProgress func(done, total int64)) (stagedPackage, error) {
		if onProgress != nil {
			onProgress(n, n)
		}
		return staged, nil
	}, nil
}

// actionReleaseUpload stages a candidate: download or upload to the spool,
// multipart-upload to the versioned key, register the row. It aims the
// package at nobody.
func (s *Server) actionReleaseUpload(sess *session, r *http.Request) error {
	product := formValue(r, "product")
	version := strings.TrimSpace(formValue(r, "version"))
	notes := formValue(r, "notes")
	minAgent := strings.TrimSpace(formValue(r, "min_agent"))
	if product != repo.ProductAgent && product != repo.ProductCodex {
		return fmt.Errorf("choose agent or codex")
	}
	if version == "" {
		return fmt.Errorf("a version is required")
	}
	// Refuse a used version before moving a single byte.
	if existing, err := s.dbm.store.Releases().ArtifactByVersion(r.Context(), product, version); err == nil {
		return fmt.Errorf("%s %s is already registered (sha256 %s…); a rebuild needs a new version", product, version, existing.SHA256[:12])
	} else if !errors.Is(err, repo.ErrNotFound) {
		return err
	}
	uploader, ok := s.dbm.objects.(packageUploader)
	if !ok {
		return fmt.Errorf("the object store cannot upload from a file")
	}
	stage, err := stagePackage(r, s.dbm.spool, maxPackageBytes)
	if err != nil {
		return err
	}
	key := artifactKey(product, version)
	actor, requestID := sess.actor, s.clientKey(r)
	ops := s.dbm.ops
	return s.jobs.start(product, version, func(setStep func(string), setProgress func(done, total int64)) error {
		setStep("获取安装包")
		staged, err := stage(setProgress)
		if err != nil {
			return err
		}
		defer staged.Remove()
		setStep("上传到 OSS")
		if err := uploader.PutFile(key, staged.Path, setProgress); err != nil {
			return err
		}
		setStep("登记版本")
		// The job outlives the request, so it gets its own context.
		_, err = ops.RegisterArtifact(context.Background(), repo.NewArtifact{
			Product: product, Version: version, SHA256: staged.SHA256, SizeBytes: staged.Size,
			ObjectKey: key, Notes: notes, Source: staged.Source, MinAgentVersion: minAgent, CreatedBy: actor,
		}, actor, requestID)
		if err != nil {
			return err
		}
		logAudit(requestID, "registered %s %s as a candidate (%d bytes, sha256 %s)", product, version, staged.Size, staged.SHA256)
		return nil
	})
}

// actionReleaseRegister registers a package that is already in the bucket at
// the conventional key -- the path CI takes, so a 700 MB installer never
// passes through this host. The object is read once to checksum it; nothing
// is written to OSS and nobody is aimed at anything.
func (s *Server) actionReleaseRegister(sess *session, r *http.Request) error {
	product := formValue(r, "product")
	version := strings.TrimSpace(formValue(r, "version"))
	notes := formValue(r, "notes")
	minAgent := strings.TrimSpace(formValue(r, "min_agent"))
	if product != repo.ProductAgent && product != repo.ProductCodex {
		return fmt.Errorf("choose agent or codex")
	}
	if version == "" {
		return fmt.Errorf("a version is required")
	}
	if existing, err := s.dbm.store.Releases().ArtifactByVersion(r.Context(), product, version); err == nil {
		return fmt.Errorf("%s %s is already registered (sha256 %s…)", product, version, existing.SHA256[:12])
	} else if !errors.Is(err, repo.ErrNotFound) {
		return err
	}
	hasher, ok := s.dbm.objects.(packageHasher)
	if !ok {
		return fmt.Errorf("the object store cannot checksum an object")
	}
	key := artifactKey(product, version)
	if _, exists, err := hasher.Head(key); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("nothing at %s; CI has not uploaded %s %s (or the version is spelled differently)", key, product, version)
	}
	actor, requestID := sess.actor, s.clientKey(r)
	ops := s.dbm.ops
	return s.jobs.start(product, version, func(setStep func(string), setProgress func(done, total int64)) error {
		setStep("校验 OSS 里的包")
		sum, size, err := hasher.HashObject(key)
		if err != nil {
			return err
		}
		setStep("登记版本")
		_, err = ops.RegisterArtifact(context.Background(), repo.NewArtifact{
			Product: product, Version: version, SHA256: sum, SizeBytes: size,
			ObjectKey: key, Notes: notes, Source: "oss:" + key, MinAgentVersion: minAgent, CreatedBy: actor,
		}, actor, requestID)
		if err != nil {
			return err
		}
		logAudit(requestID, "registered %s %s from %s as a candidate (%d bytes, sha256 %s)", product, version, key, size, sum)
		return nil
	})
}

// artifactRow is one version in the library, as the page shows it.
type artifactRow struct {
	repo.Artifact
	SizeMB   string
	IsGlobal bool
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "releases")
	data.Job = s.jobs.snapshot()
	pol, _, err := s.dbm.ops.CurrentPolicy(r.Context())
	if err != nil {
		data.Error = "could not read the policy"
	}
	data.GlobalTargets = map[string]string{repo.ProductAgent: pol.AgentUpdateVersion, repo.ProductCodex: pol.CodexVersion}
	all, err := s.dbm.store.Releases().ListArtifacts(r.Context(), "")
	if err != nil {
		data.Error = "could not list the version library"
	}
	data.Artifacts = map[string][]artifactRow{}
	for _, a := range all {
		data.Artifacts[a.Product] = append(data.Artifacts[a.Product], artifactRow{
			Artifact: a, SizeMB: fmt.Sprintf("%.1f", float64(a.SizeBytes)/(1<<20)),
			IsGlobal: data.GlobalTargets[a.Product] == a.Version,
		})
	}
	s.render(w, "releases.html", http.StatusOK, data)
}

func (s *Server) actionReleaseStatus(sess *session, r *http.Request) error {
	status := repo.ArtifactStatus(formValue(r, "status"))
	switch status {
	case repo.ArtifactAccepted, repo.ArtifactStable, repo.ArtifactRetired, repo.ArtifactCandidate:
	default:
		return fmt.Errorf("unknown status %q", status)
	}
	_, err := s.dbm.ops.SetArtifactStatus(r.Context(), formValue(r, "id"), status, formValue(r, "note"), sess.actor, s.clientKey(r))
	return err
}

func (s *Server) actionReleaseGlobal(sess *session, r *http.Request) error {
	product, version := formValue(r, "product"), formValue(r, "version")
	if err := confirmMatches(r, "confirm", version); err != nil {
		return err
	}
	if _, err := s.dbm.ops.SetGlobalTarget(r.Context(), product, version, sess.actor, s.clientKey(r)); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "aimed the fleet at %s %s", product, version)
	return nil
}

func (s *Server) actionReleaseGlobalClear(sess *session, r *http.Request) error {
	_, err := s.dbm.ops.ClearGlobalTarget(r.Context(), formValue(r, "product"), sess.actor, s.clientKey(r))
	return err
}
