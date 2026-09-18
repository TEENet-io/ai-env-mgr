// Command migrate applies the embedded schema migrations to a PostgreSQL
// database.
//
//	migrate status          # what has run, what is pending
//	migrate up              # apply everything pending
//	migrate down            # reverse the newest applied migration, one step
//
// The DSN comes from -dsn or, preferably, from AIENVMGR_DB_DSN: a connection
// string carries a password, and a password on a command line is in every
// process listing and every shell history on the host.
//
// The console applies pending migrations itself on start, so this exists for
// the first run on a fresh instance, for rollbacks, and for looking.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/TEENet-io/ai-env-mgr/db"
	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := flag.String("dsn", os.Getenv("AIENVMGR_DB_DSN"),
		"PostgreSQL connection string; prefer the AIENVMGR_DB_DSN environment variable")
	timeout := flag.Duration("timeout", 2*time.Minute, "give up after this long")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: migrate [flags] <status|up|down>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		flag.Usage()
		return errors.New("no command given")
	}
	if *dsn == "" {
		return errors.New("no database DSN: set AIENVMGR_DB_DSN or pass -dsn")
	}

	// Ctrl-C cancels the context, which aborts the statement in flight; the
	// transaction it is in rolls back, so an interrupted migration leaves the
	// schema where it was rather than half changed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	database, err := dbstore.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer database.Close()

	switch command {
	case "status":
		return status(ctx, database)
	case "up":
		pending, err := database.PendingCount(ctx)
		if err != nil {
			return err
		}
		if pending == 0 {
			fmt.Println("nothing to do: the schema is up to date")
			return nil
		}
		if err := database.Migrate(ctx); err != nil {
			return err
		}
		fmt.Printf("applied %d migration(s)\n", pending)
		return status(ctx, database)
	case "down":
		applied, err := database.Applied(ctx)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("nothing to do: no migration is applied")
			return nil
		}
		newest := applied[len(applied)-1]
		fmt.Printf("reversing %04d_%s\n", newest.Version, newest.Name)
		if err := database.MigrateDown(ctx); err != nil {
			return err
		}
		return status(ctx, database)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func status(ctx context.Context, database *dbstore.DB) error {
	all, err := db.Migrations()
	if err != nil {
		return err
	}
	applied, err := database.Applied(ctx)
	if err != nil {
		return err
	}
	when := make(map[int]time.Time, len(applied))
	for _, a := range applied {
		when[a.Version] = a.AppliedAt
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tNAME\tSTATE\tAPPLIED AT")
	for _, m := range all {
		at, ok := when[m.Version]
		state, stamp := "pending", ""
		if ok {
			state, stamp = "applied", at.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%04d\t%s\t%s\t%s\n", m.Version, m.Name, state, stamp)
	}
	return w.Flush()
}
