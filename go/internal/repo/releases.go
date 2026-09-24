package repo

import (
	"context"
	"time"
)

// Products a release can be of.
const (
	ProductAgent = "agent"
	ProductCodex = "codex"
)

// ArtifactStatus is where a package is in its life.
type ArtifactStatus string

const (
	ArtifactCandidate ArtifactStatus = "candidate"
	ArtifactAccepted  ArtifactStatus = "accepted"
	ArtifactStable    ArtifactStatus = "stable"
	ArtifactRetired   ArtifactStatus = "retired"
)

// Artifact is one package in the version library. (Product, Version) names
// exactly one SHA256, forever.
type Artifact struct {
	ID, Product, Version, SHA256 string
	SizeBytes                    int64
	ObjectKey                    string
	Status                       ArtifactStatus
	Notes, Source                string
	MinAgentVersion              string
	AcceptanceNote, AcceptedBy   string
	AcceptedAt                   *time.Time
	CreatedBy                    string
	CreatedAt, UpdatedAt         time.Time
}

// NewArtifact is what registering a package needs.
type NewArtifact struct {
	Product, Version, SHA256 string
	SizeBytes                int64
	ObjectKey                string
	Notes, Source            string
	MinAgentVersion          string
	CreatedBy                string
}

// RolloutKind says whether a rollout goes forward or back.
type RolloutKind string

const (
	RolloutRelease  RolloutKind = "release"
	RolloutRollback RolloutKind = "rollback"
)

// Rollout is one decision: this artifact, these machines.
type Rollout struct {
	ID, Product, ArtifactID string
	Kind                    RolloutKind
	RollbackOf              string // rollout id, for a rollback
	Note, CreatedBy         string
	CreatedAt               time.Time
	PausedAt                *time.Time
	PausedBy                string
}

// NewRollout is what creating a rollout needs.
type NewRollout struct {
	Product, ArtifactID string
	Kind                RolloutKind
	RollbackOf          string
	Note, CreatedBy     string
}

// TargetStatus is where one machine's target is.
type TargetStatus string

const (
	TargetPending    TargetStatus = "pending"
	TargetSucceeded  TargetStatus = "succeeded"
	TargetFailed     TargetStatus = "failed"
	TargetCancelled  TargetStatus = "cancelled"
	TargetSuperseded TargetStatus = "superseded"
	TargetExcluded   TargetStatus = "excluded"
)

// Target is the desired state of one product on one machine. It is not a
// task: nothing on the server carries it out. The machine pulls it, and its
// report settles it.
type Target struct {
	ID, DeviceID, Product, ArtifactID, RolloutID string
	Generation                                   int
	Status                                       TargetStatus
	ResultNote, ReportedVersion, ExcludeReason   string
	CreatedAt, UpdatedAt                         time.Time
	FinishedAt                                   *time.Time
}

// Releases is the version library, the rollouts and the per-machine targets.
type Releases interface {
	CreateArtifact(ctx context.Context, a NewArtifact) (Artifact, error) // ErrDuplicate on (product, version)
	ArtifactByID(ctx context.Context, id string) (Artifact, error)
	ArtifactByVersion(ctx context.Context, product, version string) (Artifact, error)
	ListArtifacts(ctx context.Context, product string) ([]Artifact, error) // newest first; "" = both products
	SetArtifactStatus(ctx context.Context, id string, status ArtifactStatus, note, by string) (Artifact, error)

	CreateRollout(ctx context.Context, r NewRollout) (Rollout, error)
	RolloutByID(ctx context.Context, id string) (Rollout, error)
	ListRollouts(ctx context.Context, limit int) ([]Rollout, error) // newest first
	SetRolloutPaused(ctx context.Context, id string, paused bool, by string) (Rollout, error)

	// CreateTarget opens a target for one device. An open target for the
	// same device and product is marked superseded first; the new one gets
	// the next generation for that pair.
	CreateTarget(ctx context.Context, deviceID, product, artifactID, rolloutID string) (Target, error)
	OpenTarget(ctx context.Context, deviceID, product string) (Target, error)
	// LastSucceededTarget is the newest target this machine took and that
	// was not released since. It is what
	// the machine is kept on once the rollout is over: with nothing said, the
	// agent would fall back to the fleet target and, if that is older,
	// install it.
	LastSucceededTarget(ctx context.Context, deviceID, product string) (Target, error)
	// ReleaseSucceeded lets go of the machine's succeeded targets for a
	// product, so the fleet target applies to it again. They stay as history.
	ReleaseSucceeded(ctx context.Context, deviceID, product string) (int, error)
	TargetByID(ctx context.Context, id string) (Target, error)
	TargetsByRollout(ctx context.Context, rolloutID string) ([]Target, error)
	TargetsByDevice(ctx context.Context, deviceID string) ([]Target, error) // newest first
	OpenTargets(ctx context.Context) ([]Target, error)
	TargetsSince(ctx context.Context, since time.Time) ([]Target, error) // for the report; newest first
	// FinishTarget moves a pending target to a terminal status. A target that
	// is not pending is left alone and ErrConflict is returned: a late
	// receipt must not rewrite a decision already recorded.
	FinishTarget(ctx context.Context, id string, status TargetStatus, note, reportedVersion string) (Target, error)
	CancelPending(ctx context.Context, rolloutID string) (int, error)
}
