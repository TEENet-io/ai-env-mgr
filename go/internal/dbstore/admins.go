package dbstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type adminRepo struct{ q querier }

const adminColumns = `id, username, email, password_hash, password_set_at,
	totp_secret, totp_key_version, totp_enrolled_at, recovery_hashes,
	role, created_at, updated_at, last_login_at, disabled_at`

func scanAdmin(row scanner) (repo.Admin, error) {
	var a repo.Admin
	var enrolled, lastLogin, disabled *time.Time
	err := row.Scan(&a.ID, &a.Username, &a.Email, &a.PasswordHash, &a.PasswordSetAt,
		&a.TOTPSecret, &a.TOTPKeyVersion, &enrolled, &a.RecoveryHashes,
		&a.Role, &a.CreatedAt, &a.UpdatedAt, &lastLogin, &disabled)
	if err != nil {
		return repo.Admin{}, err
	}
	a.TOTPEnrolledAt = enrolled
	a.LastLoginAt = lastLogin
	a.DisabledAt = disabled
	return a, nil
}

func (r adminRepo) ByID(ctx context.Context, id string) (repo.Admin, error) {
	a, err := scanAdmin(r.q.QueryRow(ctx,
		`select `+adminColumns+` from admin_principals where id = $1`, id))
	if err != nil {
		return repo.Admin{}, mapError(err, "read administrator")
	}
	return a, nil
}

func (r adminRepo) ByUsername(ctx context.Context, username string) (repo.Admin, error) {
	a, err := scanAdmin(r.q.QueryRow(ctx,
		`select `+adminColumns+` from admin_principals where username = $1`,
		normalizeUsername(username)))
	if err != nil {
		return repo.Admin{}, mapError(err, "read administrator")
	}
	return a, nil
}

func (r adminRepo) List(ctx context.Context) ([]repo.Admin, error) {
	rows, err := r.q.Query(ctx, `select `+adminColumns+` from admin_principals order by username`)
	if err != nil {
		return nil, mapError(err, "list administrators")
	}
	defer rows.Close()
	out := []repo.Admin{}
	for rows.Next() {
		a, err := scanAdmin(rows)
		if err != nil {
			return nil, mapError(err, "scan administrator")
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list administrators")
	}
	return out, nil
}

func (r adminRepo) CountEnabled(ctx context.Context) (int, error) {
	var n int
	if err := r.q.QueryRow(ctx,
		`select count(*) from admin_principals where disabled_at is null`).Scan(&n); err != nil {
		return 0, mapError(err, "count administrators")
	}
	return n, nil
}

func (r adminRepo) Create(ctx context.Context, n repo.NewAdmin) (repo.Admin, error) {
	username := normalizeUsername(n.Username)
	if username == "" {
		return repo.Admin{}, errors.New("create administrator: a user name is required")
	}
	if n.PasswordHash == "" {
		return repo.Admin{}, errors.New("create administrator: a password hash is required")
	}
	role := n.Role
	if role == "" {
		role = repo.RoleAdmin
	}
	a, err := scanAdmin(r.q.QueryRow(ctx,
		`insert into admin_principals (username, email, password_hash, role)
		 values ($1, $2, $3, $4)
		 returning `+adminColumns,
		username, n.Email, n.PasswordHash, role))
	if err != nil {
		return repo.Admin{}, mapError(err, "create administrator")
	}
	return a, nil
}

func (r adminRepo) SetPasswordHash(ctx context.Context, id, hash string) error {
	if hash == "" {
		return errors.New("set password: an empty hash would let anybody in")
	}
	tag, err := r.q.Exec(ctx,
		`update admin_principals
		    set password_hash = $2, password_set_at = now(), updated_at = now()
		  where id = $1`, id, hash)
	if err != nil {
		return mapError(err, "set password")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "set password")
	}
	return nil
}

// SetTOTP stores the sealed seed and the hashed recovery codes in one
// statement. Enrolling a second factor without recovery codes is how somebody
// locks themselves out of the console with a lost phone.
func (r adminRepo) SetTOTP(ctx context.Context, id string, sealed []byte, keyVersion string, recoveryHashes []string) error {
	if len(sealed) == 0 || keyVersion == "" {
		return errors.New("enrol second factor: the sealed seed and its key version are both required")
	}
	if len(recoveryHashes) == 0 {
		return errors.New("enrol second factor: recovery codes are required")
	}
	tag, err := r.q.Exec(ctx,
		`update admin_principals
		    set totp_secret = $2, totp_key_version = $3, totp_enrolled_at = now(),
		        recovery_hashes = $4, updated_at = now()
		  where id = $1`, id, sealed, keyVersion, recoveryHashes)
	if err != nil {
		return mapError(err, "enrol second factor")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "enrol second factor")
	}
	return nil
}

func (r adminRepo) ReplaceRecoveryHashes(ctx context.Context, id string, hashes []string) error {
	if hashes == nil {
		hashes = []string{}
	}
	tag, err := r.q.Exec(ctx,
		`update admin_principals set recovery_hashes = $2, updated_at = now() where id = $1`,
		id, hashes)
	if err != nil {
		return mapError(err, "update recovery codes")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "update recovery codes")
	}
	return nil
}

func (r adminRepo) RecordLogin(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tag, err := r.q.Exec(ctx,
		`update admin_principals set last_login_at = $2 where id = $1`, id, at.UTC())
	if err != nil {
		return mapError(err, "record sign-in")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "record sign-in")
	}
	return nil
}

func (r adminRepo) SetDisabled(ctx context.Context, id string, disabled bool) (repo.Admin, error) {
	// Disabling the last way in locks everybody out of the console, including
	// whoever would have to undo it. The check and the write are one
	// statement, so two people disabling two accounts at once cannot both pass
	// it.
	a, err := scanAdmin(r.q.QueryRow(ctx,
		`update admin_principals
		    set disabled_at = case when $2 then coalesce(disabled_at, now()) else null end,
		        updated_at = now()
		  where id = $1
		    and ($2 = false
		         or exists (select 1 from admin_principals other
		                     where other.id <> admin_principals.id and other.disabled_at is null))
		  returning `+adminColumns, id, disabled))
	if err != nil {
		if errors.Is(err, errNoRow) {
			if _, lookupErr := r.ByID(ctx, id); lookupErr == nil {
				return repo.Admin{}, errors.New("disable administrator: this is the last account that can sign in")
			}
		}
		return repo.Admin{}, mapError(err, "disable administrator")
	}
	return a, nil
}

func (r adminRepo) RecordAttempt(ctx context.Context, a repo.LoginAttempt) error {
	at := a.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if _, err := r.q.Exec(ctx,
		`insert into admin_login_attempts (username, source_ip, at, outcome)
		 values ($1, $2::inet, $3, $4)`,
		normalizeUsername(a.Username), nullable(a.SourceIP), at.UTC(), a.Outcome); err != nil {
		return mapError(err, "record sign-in attempt")
	}
	return nil
}

// RecentFailures counts failures two ways: for this account, and from this
// address. The first protects one person's password; the second is what
// notices somebody trying one password against every account.
func (r adminRepo) RecentFailures(ctx context.Context, username, sourceIP string, window time.Duration) (int, int, error) {
	if window <= 0 {
		window = 15 * time.Minute
	}
	var byUser, byIP int
	err := r.q.QueryRow(ctx,
		`select
		   count(*) filter (where username = $1),
		   count(*) filter (where source_ip = nullif($2, '')::inet)
		 from admin_login_attempts
		 where at > now() - $3::interval and outcome <> 'ok'`,
		normalizeUsername(username), sourceIP, window.String()).Scan(&byUser, &byIP)
	if err != nil {
		return 0, 0, mapError(err, "count recent sign-in failures")
	}
	return byUser, byIP, nil
}

func (r adminRepo) CreateSession(ctx context.Context, s repo.Session) error {
	if len(s.TokenSHA256) != 32 {
		return fmt.Errorf("create session: the token hash must be 32 bytes, got %d", len(s.TokenSHA256))
	}
	if s.ExpiresAt.IsZero() || s.AbsoluteExpiresAt.IsZero() {
		return errors.New("create session: both the idle and the absolute expiry are required")
	}
	if _, err := r.q.Exec(ctx,
		`insert into admin_sessions
		   (token_sha256, principal_id, expires_at, absolute_expires_at, created_ip, user_agent)
		 values ($1, $2, $3, $4, $5::inet, $6)`,
		s.TokenSHA256, s.PrincipalID, s.ExpiresAt.UTC(), s.AbsoluteExpiresAt.UTC(),
		nullable(strings.TrimSpace(s.CreatedIP)), s.UserAgent); err != nil {
		return mapError(err, "create session")
	}
	return nil
}

// SessionByToken returns a live session only.
//
// Expired, or belonging to a disabled account, is ErrNotFound: a session that
// outlives the account it belongs to is the thing being prevented, and asking
// every caller to check would eventually find one that does not.
func (r adminRepo) SessionByToken(ctx context.Context, token []byte) (repo.Session, repo.Admin, error) {
	row := r.q.QueryRow(ctx,
		`select s.token_sha256, s.principal_id, s.created_at, s.expires_at,
		        s.absolute_expires_at, s.last_seen_at, coalesce(host(s.created_ip), ''), s.user_agent,
		        `+adminColumnsPrefixed+`
		   from admin_sessions s
		   join admin_principals a on a.id = s.principal_id
		  where s.token_sha256 = $1
		    and s.expires_at > now()
		    and s.absolute_expires_at > now()
		    and a.disabled_at is null`, token)

	var s repo.Session
	var a repo.Admin
	var enrolled, lastLogin, disabled *time.Time
	err := row.Scan(&s.TokenSHA256, &s.PrincipalID, &s.CreatedAt, &s.ExpiresAt,
		&s.AbsoluteExpiresAt, &s.LastSeenAt, &s.CreatedIP, &s.UserAgent,
		&a.ID, &a.Username, &a.Email, &a.PasswordHash, &a.PasswordSetAt,
		&a.TOTPSecret, &a.TOTPKeyVersion, &enrolled, &a.RecoveryHashes,
		&a.Role, &a.CreatedAt, &a.UpdatedAt, &lastLogin, &disabled)
	if err != nil {
		return repo.Session{}, repo.Admin{}, mapError(err, "read session")
	}
	a.TOTPEnrolledAt = enrolled
	a.LastLoginAt = lastLogin
	a.DisabledAt = disabled
	return s, a, nil
}

const adminColumnsPrefixed = `a.id, a.username, a.email, a.password_hash, a.password_set_at,
	a.totp_secret, a.totp_key_version, a.totp_enrolled_at, a.recovery_hashes,
	a.role, a.created_at, a.updated_at, a.last_login_at, a.disabled_at`

func (r adminRepo) TouchSession(ctx context.Context, token []byte, expiresAt time.Time) error {
	// The absolute expiry is never moved: refreshing on activity alone would
	// let one sign-in last for ever as long as a tab stays open.
	tag, err := r.q.Exec(ctx,
		`update admin_sessions
		    set last_seen_at = now(), expires_at = least($2, absolute_expires_at)
		  where token_sha256 = $1 and expires_at > now() and absolute_expires_at > now()`,
		token, expiresAt.UTC())
	if err != nil {
		return mapError(err, "refresh session")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "refresh session")
	}
	return nil
}

func (r adminRepo) DeleteSession(ctx context.Context, token []byte) error {
	if _, err := r.q.Exec(ctx, `delete from admin_sessions where token_sha256 = $1`, token); err != nil {
		return mapError(err, "end session")
	}
	return nil
}

func (r adminRepo) DeleteSessionsFor(ctx context.Context, principalID string) (int, error) {
	tag, err := r.q.Exec(ctx, `delete from admin_sessions where principal_id = $1`, principalID)
	if err != nil {
		return 0, mapError(err, "end sessions")
	}
	return int(tag.RowsAffected()), nil
}

func (r adminRepo) DeleteExpiredSessions(ctx context.Context) (int, error) {
	tag, err := r.q.Exec(ctx,
		`delete from admin_sessions where expires_at <= now() or absolute_expires_at <= now()`)
	if err != nil {
		return 0, mapError(err, "clear expired sessions")
	}
	return int(tag.RowsAffected()), nil
}

// normalizeUsername lower-cases and trims. People type their own name with a
// capital about half the time, and "Zhang" failing to sign in while "zhang"
// works is a support call every single time.
func normalizeUsername(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func (r adminRepo) ResealTOTP(ctx context.Context, id string, sealed []byte, keyVersion string) error {
	if len(sealed) == 0 || keyVersion == "" {
		return errors.New("re-key second factor: the sealed seed and its key version are both required")
	}
	tag, err := r.q.Exec(ctx,
		`update admin_principals set totp_secret = $2, totp_key_version = $3, updated_at = now()
		  where id = $1 and totp_secret is not null`, id, sealed, keyVersion)
	if err != nil {
		return mapError(err, "re-key second factor")
	}
	if tag.RowsAffected() == 0 {
		return mapError(errNoRow, "re-key second factor")
	}
	return nil
}
