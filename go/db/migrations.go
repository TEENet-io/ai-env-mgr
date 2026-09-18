// Package db carries the SQL migrations as part of the binary.
//
// Embedding them rather than shipping a directory and a migrate CLI means the
// console can bring its own schema up to date on the host it is already
// running on, with the credentials it already has, and that a binary and its
// schema cannot be mismatched by a deploy that copied one and forgot the
// other.
package db

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Grants is the privilege grid, applied after every batch of migrations. See
// grants.sql for why it is not a migration.
//
//go:embed grants.sql
var Grants string

// Migration is one numbered step. Down may be empty for a step that cannot be
// reversed; none are today, and the runner refuses to step down past one.
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// Migrations returns every embedded migration in version order.
//
// It fails rather than guesses: a file that does not parse, a version used
// twice, or an up without a matching pair is a packaging mistake, and the
// place to find out is the first line of main, not halfway through a schema
// change on a live database.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	byVersion := map[int]*Migration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, direction, err := parseName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(migrationFiles, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		m := byVersion[version]
		if m == nil {
			m = &Migration{Version: version, Name: name}
			byVersion[version] = m
		}
		if m.Name != name {
			return nil, fmt.Errorf("migration %04d has two names: %q and %q", version, m.Name, name)
		}
		switch direction {
		case "up":
			if m.Up != "" {
				return nil, fmt.Errorf("migration %04d has two up files", version)
			}
			m.Up = string(body)
		case "down":
			if m.Down != "" {
				return nil, fmt.Errorf("migration %04d has two down files", version)
			}
			m.Down = string(body)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if strings.TrimSpace(m.Up) == "" {
			return nil, fmt.Errorf("migration %04d_%s has no up file", m.Version, m.Name)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseName splits `0001_init.up.sql` into 1, "init", "up".
func parseName(file string) (version int, name, direction string, err error) {
	base := strings.TrimSuffix(file, ".sql")
	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return 0, "", "", fmt.Errorf("migration %q: want <version>_<name>.<up|down>.sql", file)
	}
	direction = base[dot+1:]
	if direction != "up" && direction != "down" {
		return 0, "", "", fmt.Errorf("migration %q: direction must be up or down, got %q", file, direction)
	}
	rest := base[:dot]
	under := strings.Index(rest, "_")
	if under <= 0 {
		return 0, "", "", fmt.Errorf("migration %q: want <version>_<name>.<up|down>.sql", file)
	}
	version, err = strconv.Atoi(rest[:under])
	if err != nil || version <= 0 {
		return 0, "", "", fmt.Errorf("migration %q: version must be a positive number", file)
	}
	name = rest[under+1:]
	if name == "" {
		return 0, "", "", fmt.Errorf("migration %q: name must not be empty", file)
	}
	return version, name, direction, nil
}
