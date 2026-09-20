package dbstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type deviceRepo struct{ q querier }

const deviceColumns = `id, hostname, status, agent_version, note,
	last_seen_at, created_at, updated_at, revoked_at, sync_nonce`

func scanDevice(row scanner) (repo.Device, error) {
	var d repo.Device
	var status string
	var lastSeen, revoked *time.Time
	err := row.Scan(&d.ID, &d.Hostname, &status, &d.AgentVersion, &d.Note,
		&lastSeen, &d.CreatedAt, &d.UpdatedAt, &revoked, &d.SyncNonce)
	if err != nil {
		return repo.Device{}, err
	}
	d.Status = repo.DeviceStatus(status)
	d.LastSeenAt = lastSeen
	d.RevokedAt = revoked
	return d, nil
}

func (r deviceRepo) ByID(ctx context.Context, id string) (repo.Device, error) {
	d, err := scanDevice(r.q.QueryRow(ctx, `select `+deviceColumns+` from devices where id = $1`, id))
	if err != nil {
		return repo.Device{}, mapError(err, "read device")
	}
	return d, nil
}

func (r deviceRepo) ByHostname(ctx context.Context, hostname string) (repo.Device, error) {
	d, err := scanDevice(r.q.QueryRow(ctx,
		`select `+deviceColumns+` from devices where lower(hostname) = lower($1)`,
		strings.TrimSpace(hostname)))
	if err != nil {
		return repo.Device{}, mapError(err, "read device")
	}
	return d, nil
}

func (r deviceRepo) List(ctx context.Context, filter repo.DeviceFilter) ([]repo.Device, error) {
	rows, err := r.q.Query(ctx,
		`select `+deviceColumns+` from devices
		 where $1 or status <> 'revoked'
		 order by lower(hostname)`, filter.IncludeRevoked)
	if err != nil {
		return nil, mapError(err, "list devices")
	}
	defer rows.Close()
	out := []repo.Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, mapError(err, "scan device")
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list devices")
	}
	return out, nil
}

// EnsureByHostname registers a machine the first time it is seen and returns
// the existing row afterwards.
//
// The insert is the normal path rather than a read-then-insert, so two agents
// syncing at once cannot both decide the machine is new. The conflict target
// is the case-insensitive index: a machine that comes back as DESKTOP-01 after
// registering as desktop-01 is the same machine.
func (r deviceRepo) EnsureByHostname(ctx context.Context, hostname string) (repo.Device, error) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return repo.Device{}, errors.New("register device: host name must not be empty")
	}
	d, err := scanDevice(r.q.QueryRow(ctx,
		`insert into devices (hostname) values ($1)
		 on conflict (lower(hostname)) do update set updated_at = now()
		 returning `+deviceColumns, hostname))
	if err != nil {
		return repo.Device{}, mapError(err, "register device")
	}
	return d, nil
}

func (r deviceRepo) MarkSeen(ctx context.Context, id, agentVersion string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	// An empty version keeps whatever is on record: a report that does not
	// mention the version is not a report that the version is gone, and a
	// blank in the machine list reads as a broken agent.
	tag, err := r.q.Exec(ctx,
		`update devices
		    set last_seen_at = $2,
		        agent_version = case when $3 = '' then agent_version else $3 end,
		        updated_at = now()
		  where id = $1`, id, at.UTC(), agentVersion)
	if err != nil {
		return mapError(err, "record device sync")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "record device sync")
	}
	return nil
}

func (r deviceRepo) RequestSync(ctx context.Context, id, nonce string) (repo.Device, error) {
	if nonce == "" {
		return repo.Device{}, errors.New("request sync: a nonce is required")
	}
	d, err := scanDevice(r.q.QueryRow(ctx,
		`update devices set sync_nonce = $2, updated_at = now()
		  where id = $1 and status <> 'revoked'
		  returning `+deviceColumns, id, nonce))
	if err != nil {
		return repo.Device{}, mapError(err, "request sync")
	}
	return d, nil
}

func (r deviceRepo) Revoke(ctx context.Context, id string) (repo.Device, error) {
	// Revoking twice is not an error: this runs from a task, and a task runs
	// at least once.
	d, err := scanDevice(r.q.QueryRow(ctx,
		`update devices
		    set status = 'revoked',
		        revoked_at = coalesce(revoked_at, now()),
		        updated_at = now()
		  where id = $1
		  returning `+deviceColumns, id))
	if err != nil {
		return repo.Device{}, mapError(err, "revoke device")
	}
	return d, nil
}
