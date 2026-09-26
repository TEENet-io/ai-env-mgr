package repo

import (
	"context"
	"time"
)

// ApplicationTask is Admin's durable command and observed result. The agent
// may cache an installer, but never owns the task's retry or terminal state.
type ApplicationTask struct {
	ID, DeviceID, AppID, Version, State string
	AllowDowngrade                      bool
	Attempts                            int
	LeaseToken                          string
	LeaseUntil                          *time.Time
	Progress, LastError, CreatedBy      string
	CreatedAt, UpdatedAt                time.Time
	FinishedAt                          *time.Time
}

type ApplicationTasks interface {
	Create(ctx context.Context, deviceID, appID, version, actor string, allowDowngrade bool) (ApplicationTask, error)
	Cancel(ctx context.Context, id, deviceID string) (ApplicationTask, error)
	Claim(ctx context.Context, deviceID string, lease time.Duration) (ApplicationTask, error)
	// Authorize returns the task only while this device still owns a live lease.
	// It is used for task-scoped manifest/package reads; a stale task must not
	// be able to fetch an application after Admin cancelled or reassigned it.
	Authorize(ctx context.Context, id, deviceID, token string) (ApplicationTask, error)
	Renew(ctx context.Context, id, deviceID, token, progress string, lease time.Duration) (ApplicationTask, error)
	Finish(ctx context.Context, id, deviceID, token, state, lastError string) (ApplicationTask, error)
	ByID(ctx context.Context, id string) (ApplicationTask, error)
	ListRecent(ctx context.Context, limit int) ([]ApplicationTask, error)
	HasOpen(ctx context.Context, appID, version string) (bool, error)
}
