package agentcore

import (
	"errors"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// Products, as the console names them in targets and downloads.
const (
	ProductAgent = "agent"
	ProductCodex = "codex"
)

// Source is where the agent gets its instructions and sends its reports.
// The bucket is one implementation, the console's device API another; the
// sync cycle does not know which it has.
type Source interface {
	// Policy is the fleet policy and an etag that changes when it does.
	Policy() (data []byte, etag string, err error)
	// Binding is this machine's own part: assignee, targets, one-shot
	// requests. exists is false when the console has nothing machine-
	// specific to say, which the cycle reads as "not assigned yet".
	Binding(machine string) (data []byte, etag string, exists bool, err error)
	// Credentials is the assignee's bundle. With ifNoneMatch set to the etag
	// last delivered, an unchanged bundle comes back as unchanged with no
	// data -- it holds live tokens and is not moved across the network to
	// learn that nothing changed. exists false means nothing is published.
	Credentials(user, ifNoneMatch string) (data []byte, etag string, exists, unchanged bool, err error)
	// ArtifactBytes downloads a targeted build into memory (the agent
	// binary); ArtifactToFile streams one to disk (the Codex installer)
	// and returns its SHA-256.
	ArtifactBytes(product string, target model.ReleaseTarget) ([]byte, error)
	ArtifactToFile(product string, target model.ReleaseTarget, dest string) (sha256hex string, err error)
	// ReportStatus sends the machine's status; UploadLog its recent log.
	ReportStatus(machine string, data []byte) error
	UploadLog(machine string, tail []byte) error
	// Changed reports whether the policy or the binding differ from the
	// etags last seen, cheaply, for a source that has to be asked.
	Changed(machine, policySeen, bindingSeen string) (changed bool, what string)
}

// ossSource reads and writes the bucket's objects: what every agent did
// before the device API, and what a 1.3.0 agent falls back to.
type ossSource struct{ Store Store }

func (o ossSource) Policy() ([]byte, string, error) {
	return o.Store.Get(ossclient.PolicyKey())
}

func (o ossSource) Binding(machine string) ([]byte, string, bool, error) {
	data, etag, err := o.Store.Get(ossclient.BindingKey(machine))
	if errors.Is(err, ossclient.ErrNotFound) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	return data, etag, true, nil
}

func (o ossSource) Credentials(user, ifNoneMatch string) ([]byte, string, bool, bool, error) {
	key := ossclient.UserKey(user, "credentials.zip")
	etag, exists, err := o.Store.Head(key)
	if err != nil {
		return nil, "", false, false, err
	}
	if !exists {
		return nil, "", false, false, nil
	}
	if ifNoneMatch != "" && etag != "" && etag == ifNoneMatch {
		return nil, etag, true, true, nil
	}
	data, etag, err := o.Store.Get(key)
	if err != nil {
		return nil, "", false, false, err
	}
	return data, etag, true, false, nil
}

func (o ossSource) ArtifactBytes(_ string, target model.ReleaseTarget) ([]byte, error) {
	data, _, err := o.Store.Get(target.Key)
	return data, err
}

func (o ossSource) ArtifactToFile(_ string, target model.ReleaseTarget, dest string) (string, error) {
	return o.Store.GetToFile(target.Key, dest)
}

func (o ossSource) ReportStatus(machine string, data []byte) error {
	return o.Store.Put(ossclient.StatusKey(machine), data)
}

func (o ossSource) UploadLog(machine string, tail []byte) error {
	return o.Store.Put(ossclient.LogKey(machine), tail)
}

// Changed costs two HEAD requests, which is what lets the sync interval be
// long: the console's changes reach the machine on the next check instead
// of the next interval. A store that cannot be reached answers "no": the
// scheduled sync is the fallback.
func (o ossSource) Changed(machine, policySeen, bindingSeen string) (bool, string) {
	if etag, exists, err := o.Store.Head(ossclient.PolicyKey()); err == nil && exists {
		if policySeen != "" && policySeen != etag {
			return true, "policy"
		}
	}
	etag, exists, err := o.Store.Head(ossclient.BindingKey(machine))
	if err != nil {
		return false, ""
	}
	if !exists {
		etag = absentMarker
	}
	if bindingSeen != "" && bindingSeen != etag {
		return true, "binding"
	}
	return false, ""
}

// source is what the cycle talks to: the Source if one was wired, else the
// bucket through Store.
func (s *Syncer) source() Source {
	if s.Source != nil {
		return s.Source
	}
	return ossSource{Store: s.Store}
}
