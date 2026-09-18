// Package dbstore is the PostgreSQL side of the console: a connection pool, a
// transaction helper, the migration runner, and (in later files) one
// repository per entity.
//
// Everything that writes goes through Tx. The console's invariants are
// multi-row -- a re-issued credential retires the old one, supersedes the open
// tasks and enqueues new ones -- and a half-applied change is exactly the
// state that phase 0 spent its time cleaning up by hand.
package dbstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TEENet-io/ai-env-mgr/db"
)

// DB is a pool of connections plus the schema management that goes with it.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects and verifies the connection before returning.
//
// A pool that hands out its first connection lazily turns a wrong password
// into an error on some unrelated page half an hour later; the console should
// refuse to start instead.
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}
	// The console is one small service. A large pool would mostly serve to
	// exhaust the instance's connection limit during a restart loop.
	if cfg.MaxConns < 1 || cfg.MaxConns > 16 {
		cfg.MaxConns = 8
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database not reachable: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases every connection. Safe on a nil receiver so a failed startup
// path can defer it unconditionally.
func (d *DB) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

// Pool exposes the underlying pool for reads that do not need a transaction.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Ping reports whether the database answers, for the health endpoint.
func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Tx runs fn inside one transaction, committing when it returns nil and
// rolling back on error.
//
// A panic inside fn rolls back and then continues panicking: swallowing it
// would leave the caller believing its change was committed.
func (d *DB) Tx(ctx context.Context, fn func(pgx.Tx) error) (err error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback on its own context: when the caller's context is what
		// failed, a rollback on that context cannot be sent either, and the
		// connection goes back to the pool mid-transaction.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}

// migrationLockKey is the advisory lock two processes contend for before
// touching the schema. Two consoles starting at once -- a rolling deploy, or a
// human running cmd/migrate during one -- would otherwise both find the same
// migration pending and both run it.
const migrationLockKey int64 = 0x616965_6e76 // "aienv"

// AppliedMigration is one row of schema_migrations.
type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// ErrChecksumMismatch means an already-applied migration file has been edited
// since. The fix is a new migration, never a change to an old one: the
// database in front of you did not get whatever the edit added.
var ErrChecksumMismatch = errors.New("applied migration file has changed")

// Migrate applies every embedded migration that has not run yet, in order,
// each in its own transaction. It is safe to call on every start: with nothing
// pending it does nothing but take and release a lock.
func (d *DB) Migrate(ctx context.Context) error {
	pending, err := d.plan(ctx)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "select pg_advisory_unlock($1)", migrationLockKey)
	}()

	// Re-plan under the lock: whoever held it before us may have applied
	// exactly these migrations while we waited.
	pending, err = d.planOn(ctx, conn)
	if err != nil {
		return err
	}
	for _, m := range pending {
		if err := applyOne(ctx, conn, m, m.Up, true); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
		}
	}
	// A migration runs once; a grant made once does not cover a table created
	// later. Re-applying the grid here is what keeps a new table from arriving
	// unreadable by the account the console runs as.
	return applyGrants(ctx, conn)
}

// ApplyGrants re-applies the privilege grid. Migrate does this itself; the
// command exposes it for a database whose grants were changed by hand.
func (d *DB) ApplyGrants(ctx context.Context) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()
	return applyGrants(ctx, conn)
}

func applyGrants(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, db.Grants); err != nil {
		return fmt.Errorf("apply privilege grid: %w", err)
	}
	return nil
}

// MigrateDown reverses the newest applied migration, one step per call. It is
// a development and rollback tool; nothing calls it automatically.
func (d *DB) MigrateDown(ctx context.Context) error {
	applied, err := d.Applied(ctx)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		return nil
	}
	newest := applied[len(applied)-1]

	all, err := db.Migrations()
	if err != nil {
		return err
	}
	var target *db.Migration
	for i := range all {
		if all[i].Version == newest.Version {
			target = &all[i]
		}
	}
	if target == nil {
		return fmt.Errorf("migration %04d is applied but not in this binary", newest.Version)
	}
	if target.Down == "" {
		return fmt.Errorf("migration %04d_%s has no down file", target.Version, target.Name)
	}

	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "select pg_advisory_unlock($1)", migrationLockKey)
	}()

	if err := applyOne(ctx, conn, *target, target.Down, false); err != nil {
		return fmt.Errorf("migration %04d_%s down: %w", target.Version, target.Name, err)
	}
	return nil
}

// applyOne runs one migration body and records or removes its row, in a single
// transaction. A migration that fails half way leaves neither its DDL nor its
// bookkeeping behind -- PostgreSQL rolls back DDL like anything else, which is
// the reason this runner can be this short.
func applyOne(ctx context.Context, conn *pgxpool.Conn, m db.Migration, body string, up bool) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if _, err := tx.Exec(ctx, body); err != nil {
		return err
	}
	if up {
		_, err = tx.Exec(ctx,
			`insert into schema_migrations (version, name, checksum) values ($1, $2, $3)`,
			m.Version, m.Name, checksum(m.Up))
	} else {
		_, err = tx.Exec(ctx, `delete from schema_migrations where version = $1`, m.Version)
	}
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit(ctx)
}

// Applied lists what this database says has run, oldest first.
func (d *DB) Applied(ctx context.Context) ([]AppliedMigration, error) {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()
	return appliedOn(ctx, conn)
}

func appliedOn(ctx context.Context, conn *pgxpool.Conn) ([]AppliedMigration, error) {
	// The bookkeeping table is created outside any migration, because it is
	// what tells us which migrations to run.
	//
	// Looked for before it is created: CREATE TABLE IF NOT EXISTS still needs
	// CREATE on the schema even when the table is there, and the account the
	// console runs as deliberately does not have it. Only the account that
	// runs migrations creates anything.
	var exists *string
	if err := conn.QueryRow(ctx, `select to_regclass('public.schema_migrations')::text`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("look for schema_migrations: %w", err)
	}
	if exists == nil {
		if _, err := conn.Exec(ctx, `
			create table schema_migrations (
				version    integer primary key,
				name       text not null,
				checksum   text not null,
				applied_at timestamptz not null default now()
			)`); err != nil {
			return nil, fmt.Errorf("create schema_migrations: %w", err)
		}
	}
	rows, err := conn.Query(ctx,
		`select version, name, checksum, applied_at from schema_migrations order by version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// plan returns the migrations still to run, after checking that the ones
// already applied still match what this binary carries.
func (d *DB) plan(ctx context.Context) ([]db.Migration, error) {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()
	return d.planOn(ctx, conn)
}

func (d *DB) planOn(ctx context.Context, conn *pgxpool.Conn) ([]db.Migration, error) {
	all, err := db.Migrations()
	if err != nil {
		return nil, err
	}
	applied, err := appliedOn(ctx, conn)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]AppliedMigration, len(applied))
	for _, a := range applied {
		seen[a.Version] = a
	}

	var pending []db.Migration
	for _, m := range all {
		a, ok := seen[m.Version]
		if !ok {
			pending = append(pending, m)
			continue
		}
		if a.Checksum != checksum(m.Up) {
			return nil, fmt.Errorf("%w: %04d_%s", ErrChecksumMismatch, m.Version, m.Name)
		}
		delete(seen, m.Version)
	}
	// Anything left applied but unknown to this binary means an older binary
	// is being rolled back onto a newer schema. Say so; the schema is ahead,
	// and the code about to run was not written for it.
	for version := range seen {
		return nil, fmt.Errorf("database has migration %04d that this binary does not carry", version)
	}
	return pending, nil
}

// PendingCount reports how many migrations would run, for a status command
// and for a startup log line.
func (d *DB) PendingCount(ctx context.Context) (int, error) {
	pending, err := d.plan(ctx)
	if err != nil {
		return 0, err
	}
	return len(pending), nil
}

func checksum(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
