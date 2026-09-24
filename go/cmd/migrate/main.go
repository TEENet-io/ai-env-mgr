// Command migrate applies the embedded schema migrations to a PostgreSQL
// database.
//
//	migrate status          # what has run, what is pending
//	migrate up              # apply everything pending
//	migrate down            # reverse the newest applied migration, one step
//	migrate probe           # read-only: what is in the bucket and on the gateway
//	migrate import          # bring the OSS objects and gateway state into the database
//	migrate compare         # read-only: would a database in charge publish anything different? exit 2 if so
//	migrate grants          # re-apply the privilege grid (Migrate does this itself)
//
// import and compare read OSS with AIENVMGR_OSS_ACCESS_KEY_ID / _SECRET and
// the gateway with AIENVMGR_GATEWAY_URL / _ADMIN_KEY, all from the environment.
// Neither writes to OSS or to the gateway.
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
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/migrate"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
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
	bucket := flag.String("bucket", envOr("AIENVMGR_OSS_BUCKET", "ai-collect-sg"), "OSS bucket")
	endpoint := flag.String("endpoint", envOr("AIENVMGR_OSS_ENDPOINT", "oss-ap-southeast-1.aliyuncs.com"), "OSS endpoint")
	masterKey := flag.String("master-key", os.Getenv("AIENVMGR_MASTER_KEY_FILE"), "master key file, for rekey")
	dryRun := flag.Bool("dry-run", false, "rekey: count what would move, change nothing")
	pruneUnused := flag.Bool("prune-unused", false, "rekey: afterwards drop key versions nothing references from the key file")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: migrate [flags] <status|up|down|grants|probe|import|compare|rekey|lift-rate-limits>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		flag.Usage()
		return errors.New("no command given")
	}
	if command == "probe" {
		return probe(*bucket, *endpoint)
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
	case "probe":
		return probe(*bucket, *endpoint)
	case "import", "compare":
		objects, err := openOSS(*bucket, *endpoint)
		if err != nil {
			return err
		}
		store := dbstore.NewStore(database)
		var gw migrate.Gateway
		if url, key := os.Getenv("AIENVMGR_GATEWAY_URL"), os.Getenv("AIENVMGR_GATEWAY_ADMIN_KEY"); url != "" && key != "" {
			gw = litellm.New(url, key)
		}
		if command == "compare" {
			result, err := (&migrate.Comparer{Store: store, Objects: objects, Gateway: gw}).Run(ctx)
			if err != nil {
				return err
			}
			fmt.Println(result)
			if !result.Clean() {
				// The gate: a script that runs this before switching over
				// stops here, and a person reading the exit code learns the
				// same thing without parsing the text.
				os.Exit(2)
			}
			return nil
		}
		im := &migrate.Importer{Store: store, Objects: objects, Actor: "migrate import", Gateway: gw}
		report, err := im.Run(ctx)
		if err != nil {
			return err
		}
		fmt.Println(report)
		for _, w := range report.Warnings {
			fmt.Println("  warning:", w)
		}
		return nil
	case "grants":
		if err := database.ApplyGrants(ctx); err != nil {
			return err
		}
		fmt.Println("privilege grid applied")
		return nil
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
	case "rekey":
		if *masterKey == "" {
			return errors.New("rekey needs the master key file: set AIENVMGR_MASTER_KEY_FILE or pass -master-key")
		}
		ring, err := secrets.NewFileKeyring(*masterKey)
		if err != nil {
			return err
		}
		rep, err := migrate.Rekey(ctx, dbstore.NewStore(database), ring, *dryRun)
		verb := "moved"
		if *dryRun {
			verb = "would move"
		}
		fmt.Printf("current key %s: %s %d credential(s), %d administrator seed(s), %d channel secret(s)\n",
			rep.Current, verb, rep.Credentials, rep.Admins, rep.Settings)
		if err != nil {
			return err
		}
		fmt.Printf("key versions still referenced: %v\n", rep.Referenced)
		if *pruneUnused && !*dryRun {
			dropped, err := migrate.PruneKeyFile(*masterKey, rep.Referenced)
			if err != nil {
				return err
			}
			if len(dropped) == 0 {
				fmt.Println("nothing to prune: every key in the file is current or referenced")
			} else {
				fmt.Printf("dropped %v from %s (previous file kept as %s.bak)\n", dropped, *masterKey, *masterKey)
			}
		}
		return nil
	case "lift-rate-limits":
		// One-off: employees are limited by monthly budget only.
		changed, err := ops.New(dbstore.NewStore(database)).LiftRateLimits(ctx, "migrate:lift-rate-limits", "lift-rate-limits")
		for _, u := range changed {
			fmt.Printf("%s: requests, tokens and concurrency no longer limited; the Worker pushes it to the gateway\n", u)
		}
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			fmt.Println("nothing to do: every active employee is limited by budget only")
		}
		return nil
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// openOSS connects with the server identity from the environment. The key
// never appears on the command line, where every local account could read it
// off the process table.
func openOSS(bucket, endpoint string) (*ossclient.Client, error) {
	id, secret := os.Getenv("AIENVMGR_OSS_ACCESS_KEY_ID"), os.Getenv("AIENVMGR_OSS_ACCESS_KEY_SECRET")
	if id == "" || secret == "" {
		return nil, errors.New("set AIENVMGR_OSS_ACCESS_KEY_ID and AIENVMGR_OSS_ACCESS_KEY_SECRET")
	}
	return ossclient.New(endpoint, bucket, id, secret)
}

// probe says what a read of the bucket and the gateway finds, and nothing
// else. It is the first thing to run with a new identity.
func probe(bucket, endpoint string) error {
	objects, err := openOSS(bucket, endpoint)
	if err != nil {
		return err
	}
	if err := objects.Verify(); err != nil {
		return fmt.Errorf("the identity cannot read %s: %w", bucket, err)
	}
	for _, prefix := range []string{ossclient.AdminKey(""), ossclient.BindingPrefix, ossclient.StatusPrefix} {
		keys, err := objects.List(prefix)
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		fmt.Printf("%-32s %d object(s)\n", prefix, len(keys))
	}
	if _, _, err := objects.Get(ossclient.PolicyKey()); err != nil {
		fmt.Printf("%-32s %v\n", ossclient.PolicyKey(), err)
	} else {
		fmt.Printf("%-32s present\n", ossclient.PolicyKey())
	}
	if url, key := os.Getenv("AIENVMGR_GATEWAY_URL"), os.Getenv("AIENVMGR_GATEWAY_ADMIN_KEY"); url != "" && key != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		gw := litellm.New(url, key)
		users, err := gw.ListUsers(ctx)
		if err != nil {
			return fmt.Errorf("gateway users: %w", err)
		}
		keys, err := gw.ListKeys(ctx)
		if err != nil {
			return fmt.Errorf("gateway keys: %w", err)
		}
		fmt.Printf("%-32s %d user(s), %d key(s)\n", "gateway", len(users), len(keys))
	} else {
		fmt.Println("gateway: not configured (AIENVMGR_GATEWAY_URL / _ADMIN_KEY), skipped")
	}
	return nil
}
