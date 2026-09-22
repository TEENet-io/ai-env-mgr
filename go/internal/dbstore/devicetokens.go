package dbstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type deviceTokenRepo struct{ q querier }

// newToken is 32 random bytes as URL-safe base64: what the agent stores
// and sends. Only its hash is written down here.
func newToken() (plaintext string, hash []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	plaintext = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, sum[:], nil
}

func hashToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

func (r deviceTokenRepo) Issue(ctx context.Context, deviceID string) (string, error) {
	return r.mint(ctx, deviceID, 0)
}

func (r deviceTokenRepo) Rotate(ctx context.Context, deviceID string, grace time.Duration) (string, error) {
	if grace <= 0 {
		grace = 10 * time.Minute
	}
	return r.mint(ctx, deviceID, grace)
}

// mint ends the device's live tokens -- now, or after grace -- and inserts
// a new one. Both statements run on the same querier, so inside a
// transaction they are one change.
func (r deviceTokenRepo) mint(ctx context.Context, deviceID string, grace time.Duration) (string, error) {
	if deviceID == "" {
		return "", errors.New("issue device token: a device is required")
	}
	if grace == 0 {
		if _, err := r.q.Exec(ctx,
			`update device_tokens set revoked_at = now() where device_id = $1 and revoked_at is null`, deviceID); err != nil {
			return "", mapError(err, "retire device tokens")
		}
	} else {
		if _, err := r.q.Exec(ctx,
			`update device_tokens set grace_until = now() + $2
			  where device_id = $1 and revoked_at is null and grace_until is null`, deviceID, grace); err != nil {
			return "", mapError(err, "retire device tokens")
		}
	}
	plaintext, hash, err := newToken()
	if err != nil {
		return "", err
	}
	if _, err := r.q.Exec(ctx,
		`insert into device_tokens (device_id, token_hash) values ($1, $2)`, deviceID, hash); err != nil {
		return "", mapError(err, "issue device token")
	}
	return plaintext, nil
}

func (r deviceTokenRepo) Authenticate(ctx context.Context, plaintext string, now time.Time) (repo.Device, error) {
	if plaintext == "" {
		return repo.Device{}, mapError(errNoRow, "authenticate device")
	}
	d, err := scanDevice(r.q.QueryRow(ctx,
		`update device_tokens t set last_used_at = $2
		   from devices d
		  where t.token_hash = $1 and t.device_id = d.id
		    and t.revoked_at is null and (t.grace_until is null or t.grace_until > $2)
		    and d.status <> 'revoked'
		  returning `+deviceColumnsPrefixed("d"), hashToken(plaintext), now.UTC()))
	if err != nil {
		return repo.Device{}, mapError(err, "authenticate device")
	}
	return d, nil
}

func (r deviceTokenRepo) Revoke(ctx context.Context, deviceID string) (int, error) {
	tag, err := r.q.Exec(ctx,
		`update device_tokens set revoked_at = now() where device_id = $1 and revoked_at is null`, deviceID)
	if err != nil {
		return 0, mapError(err, "revoke device tokens")
	}
	return int(tag.RowsAffected()), nil
}

func (r deviceTokenRepo) HasLive(ctx context.Context, deviceID string, now time.Time) (bool, error) {
	var n int
	err := r.q.QueryRow(ctx,
		`select count(*) from device_tokens
		  where device_id = $1 and revoked_at is null and (grace_until is null or grace_until > $2)`,
		deviceID, now.UTC()).Scan(&n)
	if err != nil {
		return false, mapError(err, "check device token")
	}
	return n > 0, nil
}

func (r deviceTokenRepo) IssuedAt(ctx context.Context, deviceID string) (time.Time, error) {
	var at time.Time
	err := r.q.QueryRow(ctx,
		`select created_at from device_tokens
		  where device_id = $1 and revoked_at is null and grace_until is null
		  order by created_at desc limit 1`, deviceID).Scan(&at)
	if err != nil {
		return time.Time{}, mapError(err, "read device token")
	}
	return at, nil
}

// deviceColumnsPrefixed qualifies the device columns with a table alias, for
// a query that joins devices with something else.
func deviceColumnsPrefixed(alias string) string {
	return alias + `.id, ` + alias + `.hostname, ` + alias + `.status, ` + alias + `.agent_version, ` + alias + `.note,
	` + alias + `.last_seen_at, ` + alias + `.created_at, ` + alias + `.updated_at, ` + alias + `.revoked_at, ` + alias + `.sync_nonce,
	` + alias + `.channel, ` + alias + `.enrolled_at, ` + alias + `.enrolled_from, ` + alias + `.reenrol_allowed_until, ` + alias + `.log_tail, ` + alias + `.log_tail_at`
}
