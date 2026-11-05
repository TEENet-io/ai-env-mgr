// Command tenant-admin lets each company's console manage its own team on
// the shared LiteLLM gateway, and nothing else. It runs beside LiteLLM, under
// litellm.teenet.app/tenant-admin/, and holds the gateway administrator key
// so that no console has to.
//
//	tenant-admin serve                       # the HTTP service
//	tenant-admin token new  -team wewe       # create the team if needed, issue a token
//	tenant-admin token list                  # tenants and token ids (never tokens)
//	tenant-admin token revoke -id wewe-1     # remove one token
//	tenant-admin probe                       # exit 0 when the running service is healthy (container healthcheck)
//
// Everything that changes who may call is a command run on the gateway host
// (docker exec tenant-admin tenant-admin ...): the service has no
// management endpoint at all.
//
// Environment:
//
//	TENANT_ADMIN_CONFIG       tenants.json (default /config/tenants.json)
//	TENANT_ADMIN_LISTEN       serve: listen address (default :4100)
//	TENANT_ADMIN_LITELLM_URL  LiteLLM, from inside the compose network (default http://litellm:4000)
//	TENANT_ADMIN_LITELLM_KEY  the administrator key tenant-admin uses on LiteLLM
//
// Design: docs/superpowers/specs/2026-11-05-tenant-admin-proxy-design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/tenantadmin"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tenant-admin:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	configPath := envOr("TENANT_ADMIN_CONFIG", "/config/tenants.json")
	if len(args) == 0 {
		return errors.New("usage: tenant-admin serve | token new -team <team> | token list | token revoke -id <id>")
	}
	switch args[0] {
	case "serve":
		return serve(configPath)
	case "probe":
		return probe()
	case "token":
		if len(args) < 2 {
			return errors.New("usage: tenant-admin token new -team <team> | list | revoke -id <id>")
		}
		return token(configPath, args[1], args[2:])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func serve(configPath string) error {
	key := os.Getenv("TENANT_ADMIN_LITELLM_KEY")
	if key == "" {
		return errors.New("set TENANT_ADMIN_LITELLM_KEY")
	}
	initial, err := tenantadmin.LoadConfig(configPath)
	if err != nil {
		return err
	}
	var current atomic.Pointer[tenantadmin.Config]
	current.Store(&initial)

	// Reload on SIGHUP, and on a change to the file: "token new" run with
	// docker exec is another process and cannot signal this one's PID
	// reliably. A file that fails to load keeps the previous configuration.
	reload := func(why string) {
		c, err := tenantadmin.LoadConfig(configPath)
		if err != nil {
			log.Printf(`{"event":"reload_failed","reason":%q,"error":%q}`, why, err.Error())
			return
		}
		current.Store(&c)
		log.Printf(`{"event":"reloaded","reason":%q,"tenants":%d}`, why, len(c.Tenants))
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		var last time.Time
		if fi, err := os.Stat(configPath); err == nil {
			last = fi.ModTime()
		}
		tick := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-hup:
				reload("sighup")
			case <-tick.C:
				if fi, err := os.Stat(configPath); err == nil && !fi.ModTime().Equal(last) {
					last = fi.ModTime()
					reload("file changed")
				}
			}
		}
	}()

	logger := log.New(os.Stdout, "", 0)
	srv := &http.Server{
		Addr: envOr("TENANT_ADMIN_LISTEN", ":4100"),
		Handler: &tenantadmin.Server{
			Upstream: tenantadmin.HTTPUpstream{BaseURL: envOr("TENANT_ADMIN_LITELLM_URL", "http://litellm:4000"), AdminKey: key},
			Config:   func() tenantadmin.Config { return *current.Load() },
			Log:      logger,
		},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf(`{"event":"listening","addr":%q,"tenants":%d}`, srv.Addr, len(initial.Tenants))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// probe asks the service in this container for /healthz. The image has no
// shell or curl, so the compose healthcheck runs this.
func probe() error {
	addr := envOr("TENANT_ADMIN_LISTEN", ":4100")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %d", resp.StatusCode)
	}
	return nil
}

func token(configPath, verb string, args []string) error {
	fs := flag.NewFlagSet("token "+verb, flag.ContinueOnError)
	team := fs.String("team", "", "token new: the team, e.g. wewe")
	id := fs.String("id", "", "token revoke: the token id, as token list shows it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := tenantadmin.LoadConfig(configPath)
	if err != nil {
		return err
	}
	switch verb {
	case "new":
		if err := litellm.ValidateTeamID(*team); err != nil || *team == "" {
			return fmt.Errorf("-team: use 2-32 lower-case letters, digits or underscores")
		}
		// The team is made here, on the host, and never through the
		// service: no tenant can create a team.
		key := os.Getenv("TENANT_ADMIN_LITELLM_KEY")
		if key == "" {
			return errors.New("set TENANT_ADMIN_LITELLM_KEY")
		}
		gw := litellm.New(envOr("TENANT_ADMIN_LITELLM_URL", "http://litellm:4000"), key)
		gw.TeamID = *team
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := gw.EnsureTeam(ctx); err != nil {
			return err
		}
		plain, digest, err := tenantadmin.NewToken()
		if err != nil {
			return err
		}
		tokenID, err := c.AddToken(*team, digest)
		if err != nil {
			return err
		}
		if err := c.Save(configPath); err != nil {
			return err
		}
		fmt.Printf("team:  %s (exists on the gateway)\ntoken id: %s\ntoken: %s\n\n", *team, tokenID, plain)
		fmt.Println("The token is shown this once. Put it in the console's AIENVMGR_GATEWAY_ADMIN_KEY;")
		fmt.Println("the service picks the new token up within 5 seconds.")
		return nil
	case "list":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TEAM\tENABLED\tTOKEN IDS")
		for _, t := range c.Tenants {
			ids := ""
			for i, tok := range t.Tokens {
				if i > 0 {
					ids += ", "
				}
				ids += tok.ID
			}
			fmt.Fprintf(w, "%s\t%v\t%s\n", t.Team, t.Enabled, ids)
		}
		return w.Flush()
	case "revoke":
		if err := c.RemoveToken(*id); err != nil {
			return err
		}
		if err := c.Save(configPath); err != nil {
			return err
		}
		fmt.Printf("token %s removed; the service stops accepting it within 5 seconds\n", *id)
		return nil
	}
	return fmt.Errorf("unknown token command %q", verb)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
