package dbstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/db"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// testDB opens the database named by TEST_PG_DSN, or skips.
//
// The DSN must point at a database nobody minds losing: every test here starts
// by dropping the schema. Locally:
//
//	docker exec aienv-pg15 createdb -U postgres aienv_test
//	TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/dbstore/
func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN is not set; skipping the PostgreSQL integration tests")
	}
	ctx := context.Background()
	// A database of this package's own: `go test ./...` runs packages in
	// parallel and each of these suites empties the schema it is about to use.
	dsn, err := TestDatabaseDSN(ctx, dsn, "aienv_test_dbstore")
	if err != nil {
		t.Fatalf("test database: %v", err)
	}
	database, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(database.Close)

	if _, err := database.pool.Exec(ctx, `drop schema public cascade; create schema public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	// The role grants in 0002 apply to the schema, which was just replaced.
	if _, err := database.pool.Exec(ctx, `grant all on schema public to public`); err != nil {
		t.Fatalf("restore default schema grant: %v", err)
	}
	return database
}

func TestEveryMigrationHasAMatchingPair(t *testing.T) {
	all, err := db.Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations are embedded; the binary would create an empty schema")
	}
	for i, m := range all {
		if i > 0 && all[i-1].Version >= m.Version {
			t.Fatalf("migrations are not in ascending order: %d then %d", all[i-1].Version, m.Version)
		}
		if strings.TrimSpace(m.Up) == "" {
			t.Errorf("migration %04d_%s has an empty up", m.Version, m.Name)
		}
		// A migration with no way back is one that has to be undone by hand,
		// at the worst possible moment.
		if strings.TrimSpace(m.Down) == "" {
			t.Errorf("migration %04d_%s has no down file", m.Version, m.Name)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	applied, err := database.Applied(ctx)
	if err != nil {
		t.Fatalf("applied: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("nothing recorded in schema_migrations after a successful migrate")
	}
	firstRun := applied[0].AppliedAt

	// The console migrates on every start; a second start must be a no-op
	// rather than a re-run.
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	again, err := database.Applied(ctx)
	if err != nil {
		t.Fatalf("applied again: %v", err)
	}
	if len(again) != len(applied) {
		t.Fatalf("second migrate changed the applied set: %d then %d", len(applied), len(again))
	}
	if !again[0].AppliedAt.Equal(firstRun) {
		t.Error("second migrate re-applied migration 1; it should have been skipped")
	}
	pending, err := database.PendingCount(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending = %d after migrating, want 0", pending)
	}
}

func TestMigrateDownThenUp(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	all, err := db.Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}

	for range all {
		if err := database.MigrateDown(ctx); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
	}
	applied, err := database.Applied(ctx)
	if err != nil {
		t.Fatalf("applied: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("after stepping all the way down, %d migrations are still recorded", len(applied))
	}
	var exists bool
	if err := database.pool.QueryRow(ctx,
		`select exists (select 1 from information_schema.tables
		                where table_schema = 'public' and table_name = 'employees')`).Scan(&exists); err != nil {
		t.Fatalf("check employees: %v", err)
	}
	if exists {
		t.Error("employees survived the down migration")
	}

	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if err := database.pool.QueryRow(ctx,
		`select exists (select 1 from information_schema.tables
		                where table_schema = 'public' and table_name = 'employees')`).Scan(&exists); err != nil {
		t.Fatalf("check employees: %v", err)
	}
	if !exists {
		t.Error("employees is missing after migrating up a second time")
	}
}

func TestMigrateRefusesAnEditedMigration(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Stand in for "somebody edited 0001 after it shipped": the file this
	// binary carries no longer matches what this database ran.
	if _, err := database.pool.Exec(ctx,
		`update schema_migrations set checksum = 'not-the-checksum' where version = 1`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := database.Migrate(ctx)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Migrate error = %v, want ErrChecksumMismatch", err)
	}
}

func TestMigrateRefusesASchemaFromTheFuture(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A newer binary migrated this database, then was rolled back to this one.
	// Running on a schema we do not know is how a "column does not exist" gets
	// discovered by an employee instead of by a deploy.
	if _, err := database.pool.Exec(ctx,
		`insert into schema_migrations (version, name, checksum) values (9999, 'from-the-future', 'x')`); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	err := database.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "9999") {
		t.Fatalf("Migrate error = %v, want a complaint about migration 9999", err)
	}
}

func TestTxCommitsAndRollsBack(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := database.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into employees (windows_user, name) values ('work1', 'kept')`)
		return err
	}); err != nil {
		t.Fatalf("committing transaction: %v", err)
	}

	wantErr := errors.New("the caller changed its mind")
	err := database.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`insert into employees (windows_user, name) values ('work2', 'discarded')`); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Tx error = %v, want the caller's error back unwrapped", err)
	}

	var users []string
	rows, err := database.pool.Query(ctx, `select windows_user from employees order by windows_user`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan: %v", err)
		}
		users = append(users, u)
	}
	if len(users) != 1 || users[0] != "work1" {
		t.Fatalf("employees = %v, want only work1: the failed transaction was not rolled back", users)
	}
}

func TestTxRollsBackOnPanic(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed; the caller would think the write succeeded")
			}
		}()
		_ = database.Tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				`insert into employees (windows_user) values ('work3')`); err != nil {
				t.Errorf("insert: %v", err)
			}
			panic("a bug in the middle of a transaction")
		})
	}()

	// Give the deferred rollback its own moment; it runs on a fresh context.
	ctxAfter, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var count int
	if err := database.pool.QueryRow(ctxAfter,
		`select count(*) from employees where windows_user = 'work3'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Error("the row written before the panic is still there")
	}
}

// The append-only rule is a database grant, not a convention in the Go code:
// it has to hold for a bug and for a stolen application password alike.
func TestApplicationRoleCannotRewriteAudit(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	conn, err := database.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `set role aienv_app`); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`insert into audit_events (actor_type, actor_id, action, target_type, target_id)
		 values ('admin', 'tester', 'account.onboard', 'employee', 'work1')`); err != nil {
		t.Fatalf("the application role must be able to append: %v", err)
	}
	// An attempt is opened when a task is claimed and closed when it
	// finishes, so the row is written twice. A grid that forbade the second
	// write left every task "running" forever with its result unrecorded.
	if _, err := conn.Exec(ctx, `insert into tasks (kind, idempotency_key) values ('t', 'k')`); err != nil {
		t.Fatalf("the application role must be able to enqueue: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`insert into task_attempts (task_id, attempt) select id, 1 from tasks where idempotency_key = 'k'`); err != nil {
		t.Fatalf("the application role must be able to open an attempt: %v", err)
	}
	if _, err := conn.Exec(ctx, `update task_attempts set outcome = 'succeeded', ended_at = now()`); err != nil {
		t.Fatalf("the application role must be able to close an attempt: %v", err)
	}
	for _, statement := range []string{
		`update audit_events set result = 'tampered'`,
		`delete from audit_events`,
		`delete from task_attempts`,
		`create table sneaky (x int)`,
	} {
		if _, err := conn.Exec(ctx, statement); err == nil {
			t.Errorf("the application role was allowed to run: %s", statement)
		}
	}
	if _, err := conn.Exec(ctx, `reset role`); err != nil {
		t.Fatalf("reset role: %v", err)
	}
}

// A migration runs once; a grant made once does not cover a table created
// later. The grid has to be re-applied, or the next table arrives unreadable
// by the account the console runs as.
func TestGrantsCoverTablesCreatedLater(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := database.pool.Exec(ctx, `create table late_arrival (x int)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := database.ApplyGrants(ctx); err != nil {
		t.Fatalf("grants: %v", err)
	}

	conn, err := database.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `set role aienv_app`); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := conn.Exec(ctx, `select * from late_arrival`); err != nil {
		t.Errorf("the application role cannot read a table created after the migrations: %v", err)
	}
	// The record of which schema the code is running on is not the code's
	// to rewrite.
	for _, statement := range []string{
		`update schema_migrations set checksum = 'x'`,
		`delete from schema_migrations`,
		`insert into schema_migrations (version, name, checksum) values (9, 'x', 'y')`,
	} {
		if _, err := conn.Exec(ctx, statement); err == nil {
			t.Errorf("the application role was allowed to run: %s", statement)
		}
	}
	if _, err := conn.Exec(ctx, `select count(*) from schema_migrations`); err != nil {
		t.Errorf("the application role cannot read the migration record: %v", err)
	}
}

func TestLockSerialisesAcrossTransactions(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewStore(database)

	// A lock that is released the moment the statement ends protects
	// nothing, so outside a transaction it is refused.
	if err := store.Lock(ctx, "x"); err == nil {
		t.Fatal("Lock outside a transaction was allowed")
	}

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = store.InTx(ctx, func(tx repo.Store) error {
			if err := tx.Lock(ctx, "oss_export:employee:1"); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// The second taker waits, rather than proceeding to interleave its writes.
	acquired := make(chan struct{})
	go func() {
		_ = store.InTx(ctx, func(tx repo.Store) error {
			if err := tx.Lock(ctx, "oss_export:employee:1"); err != nil {
				return err
			}
			close(acquired)
			return nil
		})
	}()
	select {
	case <-acquired:
		t.Fatal("the second transaction took the lock while the first still held it")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the second transaction never got the lock after the first released it")
	}
}
