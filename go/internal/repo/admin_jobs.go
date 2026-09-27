package repo

import (
	"context"
	"time"
)

// AdminJob is a long-running operation started by the console itself rather
// than by the device Worker. It is deliberately separate from Task: the
// operation owns its goroutine and progress callback, while Task is leased to
// a Worker handler.
type AdminJob struct {
	ID         string
	Kind       string
	Version    string
	State      string
	Step       string
	DoneBytes  int64
	TotalBytes int64
	LastError  string
	StartedAt  time.Time
	EndedAt    *time.Time
	UpdatedAt  time.Time
}

type AdminJobs interface {
	Create(ctx context.Context, id, kind, version string) (AdminJob, error)
	Update(ctx context.Context, id, state, step string, doneBytes, totalBytes int64, lastError string, endedAt *time.Time) error
	Latest(ctx context.Context) (AdminJob, error)
	ListRecent(ctx context.Context, limit int) ([]AdminJob, error)
	RecoverRunning(ctx context.Context, reason string) (int, error)
}
