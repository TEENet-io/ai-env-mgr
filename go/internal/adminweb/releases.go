package adminweb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

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

// packageRow is an object in the bucket at one of the version library's
// keys that no artifact row names yet: something CI put there, waiting to be
// registered. The version is read off the key.
type packageRow struct {
	Product, Version, Key string
	SizeMB                string
	Modified              time.Time
}

// versionFromKey reads the version out of a version-library key, or "".
func versionFromKey(product, key string) string {
	if product == repo.ProductCodex {
		return ossclient.CodexVersionFromKey(key)
	}
	return ossclient.AgentVersionFromKey(key)
}

// unregisteredPackages lists what is in the bucket under the library's keys
// and not yet in the library. Read-only: the page shows it with a
// one-click "登记".
func (s *Server) unregisteredPackages(ctx context.Context, known []repo.Artifact) ([]packageRow, error) {
	registered := map[string]bool{}
	for _, a := range known {
		registered[a.Product+"/"+a.Version] = true
	}
	var rows []packageRow
	for _, product := range []string{repo.ProductCodex, repo.ProductAgent} {
		prefix := ossclient.CodexPrefix
		if product == repo.ProductAgent {
			prefix = ossclient.AgentPrefix
		}
		objects, err := s.dbm.objects.ListInfo(prefix)
		if err != nil {
			return nil, err
		}
		for _, o := range objects {
			version := versionFromKey(product, o.Key)
			if version == "" || registered[product+"/"+version] {
				continue
			}
			rows = append(rows, packageRow{Product: product, Version: version, Key: o.Key,
				SizeMB: fmt.Sprintf("%.1f", float64(o.Size)/(1<<20)), Modified: o.LastModified})
		}
	}
	return rows, nil
}

// artifactSeverity maps a version's status onto the tag colours the console
// uses everywhere: stable is good, accepted is on its way, a candidate is
// neutral, retired is out.
func artifactSeverity(status repo.ArtifactStatus) string {
	switch status {
	case repo.ArtifactStable:
		return "ok"
	case repo.ArtifactAccepted:
		return "warn"
	case repo.ArtifactRetired:
		return "bad"
	}
	return "muted"
}

// artifactLabel is the status in the operator's language.
func artifactLabel(severity string) string {
	switch severity {
	case "ok":
		return "稳定"
	case "warn":
		return "已验收"
	case "bad":
		return "已停用"
	}
	return "候选"
}

// artifactRow is one version in the library, as the page shows it.
type artifactRow struct {
	repo.Artifact
	SizeMB   string
	IsGlobal bool
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "releases")
	data.Tab = "releases"
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
	if data.Unregistered, err = s.unregisteredPackages(r.Context(), all); err != nil {
		data.Error = "could not list the bucket: " + err.Error()
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

func (s *Server) actionReleaseNotes(sess *session, r *http.Request) error {
	return s.dbm.ops.SetArtifactNotes(r.Context(), formValue(r, "id"), formValue(r, "notes"), sess.actor, s.clientKey(r))
}
