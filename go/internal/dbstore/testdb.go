package dbstore

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// safeDatabaseName is what may appear in a CREATE DATABASE. The name cannot be
// a parameter in that statement, so it is checked rather than escaped.
var safeDatabaseName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// TestDatabaseDSN returns a DSN for a database of the given name, creating it
// if it is not there, and is meant only for tests.
//
// Each test package needs its own database. `go test ./...` runs packages in
// parallel, every one of these suites starts by emptying the schema it is
// about to use, and pointed at one shared database they delete each other's
// tables half way through a run -- which looks like a flaky test and is not.
//
// It lives in the package rather than in a test file so that the packages
// built on top of dbstore can use it without importing their own dependency
// backwards.
func TestDatabaseDSN(ctx context.Context, adminDSN, name string) (string, error) {
	if !safeDatabaseName.MatchString(name) {
		return "", fmt.Errorf("test database name %q must be lower-case letters, digits and underscores", name)
	}
	admin, err := Open(ctx, adminDSN)
	if err != nil {
		return "", err
	}
	defer admin.Close()

	var exists bool
	if err := admin.pool.QueryRow(ctx,
		`select exists (select 1 from pg_database where datname = $1)`, name).Scan(&exists); err != nil {
		return "", fmt.Errorf("look for the test database: %w", err)
	}
	if !exists {
		if _, err := admin.pool.Exec(ctx, `create database `+name); err != nil {
			// Another package's suite may have created it a moment ago, which
			// is a race worth losing quietly.
			if !strings.Contains(err.Error(), "already exists") {
				return "", fmt.Errorf("create the test database %s: %w", name, err)
			}
		}
	}
	return replaceDatabase(adminDSN, name)
}

// replaceDatabase swaps the database out of a connection string, keeping the
// host, the credentials and every parameter.
func replaceDatabase(dsn, name string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse the test DSN: %w", err)
		}
		u.Path = "/" + name
		return u.String(), nil
	}
	// Key/value form: replace dbname= if it is there, append it otherwise.
	fields := strings.Fields(dsn)
	replaced := false
	for i, f := range fields {
		if strings.HasPrefix(f, "dbname=") {
			fields[i] = "dbname=" + name
			replaced = true
		}
	}
	if !replaced {
		fields = append(fields, "dbname="+name)
	}
	return strings.Join(fields, " "), nil
}
