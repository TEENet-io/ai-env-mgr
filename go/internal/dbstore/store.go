package dbstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// querier is the part of the pgx API that a repository needs. Both the pool
// and a transaction satisfy it, which is what lets every repository method be
// written once and run either on its own or inside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the PostgreSQL implementation of repo.Store.
//
// The zero-argument accessors return small structs that share this Store's
// querier, so Employees() inside a transaction is transactional and the same
// call on the pool is not -- without the caller having to remember which.
type Store struct {
	db *DB
	q  querier
	// inTx marks a Store that is already inside a transaction, so a nested
	// InTx can refuse instead of quietly running outside it.
	inTx bool
}

// NewStore wraps a connected database.
func NewStore(db *DB) *Store { return &Store{db: db, q: db.pool} }

func (s *Store) Employees() repo.Employees { return employeeRepo{s.q} }
func (s *Store) Quotas() repo.Quotas       { return quotaRepo{s.q} }
func (s *Store) Devices() repo.Devices     { return deviceRepo{s.q} }
func (s *Store) Bindings() repo.Bindings   { return bindingRepo{s.q} }
func (s *Store) Policies() repo.Policies   { return policyRepo{s.q} }
func (s *Store) Settings() repo.Settings   { return settingsRepo{s.q} }
func (s *Store) Tasks() repo.Tasks         { return taskRepo{s.q} }
func (s *Store) Audit() repo.Audit         { return auditRepo{s.q} }
func (s *Store) Grants() repo.Grants       { return grantRepo{s.q} }

func (s *Store) Credentials() repo.Credentials             { return credentialRepo{s.q} }
func (s *Store) Admins() repo.Admins                       { return adminRepo{s.q} }
func (s *Store) LegacyIDs() repo.LegacyIDs                 { return legacyRepo{s.q} }
func (s *Store) Reports() repo.Reports                     { return reportRepo{s.q} }
func (s *Store) Releases() repo.Releases                   { return releaseRepo{s.q} }
func (s *Store) Usage() repo.Usage                         { return usageRepo{s.q} }
func (s *Store) Alerts() repo.Alerts                       { return alertRepo{s.q} }
func (s *Store) DeviceTokens() repo.DeviceTokens           { return deviceTokenRepo{s.q} }
func (s *Store) CredentialBundles() repo.CredentialBundles { return bundleRepo{s.q} }
func (s *Store) ApplicationTasks() repo.ApplicationTasks {
	return applicationTaskRepo{s.q, s.db, s.inTx}
}

// errNoRow is what an Exec that matched nothing reports, so that a statement
// written as an UPDATE goes through the same mapping as one written with
// RETURNING and both come out as repo.ErrNotFound.
var errNoRow = pgx.ErrNoRows

// InTx runs fn against a transactional Store.
//
// Nesting is refused rather than flattened: a caller that thinks it opened a
// transaction, and did not, gets a partial write on the first error -- the
// exact failure this whole layer exists to prevent.
func (s *Store) InTx(ctx context.Context, fn func(repo.Store) error) error {
	if s.inTx {
		return errors.New("dbstore: InTx called inside a transaction")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		return fn(&Store{db: s.db, q: tx, inTx: true})
	})
}

// Lock takes a transaction-scoped advisory lock on name. Refused outside a
// transaction, because a lock that is released the moment the statement ends
// protects nothing and would only look as if it did.
func (s *Store) Lock(ctx context.Context, name string) error {
	if !s.inTx {
		return errors.New("dbstore: Lock called outside a transaction")
	}
	if _, err := s.q.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, name); err != nil {
		return mapError(err, "lock "+name)
	}
	return nil
}

// mapError turns a PostgreSQL error into one of the repo sentinels, so callers
// can tell "somebody else got there first" from "the database is down" without
// reading error strings.
//
// The constraint name is kept in the message: "already exists" on its own,
// when three unique indexes could have fired, costs a person half an hour.
func mapError(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, repo.ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%s: %w (%s)", what, repo.ErrDuplicate, pgErr.ConstraintName)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%s: referenced row is missing: %w (%s)", what, repo.ErrNotFound, pgErr.ConstraintName)
		case "23514": // check_violation
			return fmt.Errorf("%s: rejected by constraint %s", what, pgErr.ConstraintName)
		case "22P02": // invalid_text_representation, e.g. a malformed uuid
			return fmt.Errorf("%s: %w", what, repo.ErrNotFound)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
