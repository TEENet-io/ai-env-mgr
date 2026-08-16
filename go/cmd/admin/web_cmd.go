package main

import (
	"fmt"
	"os"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/adminweb"
)

func cmdWeb(args []string) error {
	// Reuse the location baked into this binary, exactly as the CLI does, so
	// the console asks only for the AccessKey.
	built := builtIn()
	opts := adminweb.Options{
		Listen:   "127.0.0.1:8080",
		Bucket:   built.Bucket,
		Endpoint: built.Endpoint,
		// The environment wins over the built-in value, so the key can be
		// rotated without a rebuild.
		ECDAccessKeyID:     envOr("AIENVMGR_ECD_KEY_ID", ecdAccessKeyID),
		ECDAccessKeySecret: envOr("AIENVMGR_ECD_KEY_SECRET", ecdAccessKeySecret),
		ECDRegion:          envOr("AIENVMGR_ECD_REGION", ecdRegion),
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--listen":
			if i+1 >= len(args) {
				return fmt.Errorf("--listen needs a value")
			}
			opts.Listen = args[i+1]
			i++
		case "--cert":
			if i+1 >= len(args) {
				return fmt.Errorf("--cert needs a value")
			}
			opts.CertFile = args[i+1]
			i++
		case "--key":
			if i+1 >= len(args) {
				return fmt.Errorf("--key needs a value")
			}
			opts.KeyFile = args[i+1]
			i++
		case "--behind-proxy":
			opts.BehindProxy = true
		case "--idle-timeout":
			if i+1 >= len(args) {
				return fmt.Errorf("--idle-timeout needs a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return fmt.Errorf("--idle-timeout: %w", err)
			}
			opts.IdleTTL = d
			i++
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if (opts.CertFile == "") != (opts.KeyFile == "") {
		return fmt.Errorf("--cert and --key go together")
	}

	srv, err := adminweb.New(opts)
	if err != nil {
		return err
	}
	scheme := "http"
	if opts.CertFile != "" {
		scheme = "https"
	}
	fmt.Printf("admin console on %s://%s\n", scheme, opts.Listen)
	fmt.Println("sign in with the OSS credentials; they stay in this process's memory only.")
	fmt.Println("stopping the server signs everyone out.")
	return srv.ListenAndServe()
}

// envOr prefers an environment variable, falling back to what was built in.
func envOr(key, built string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return built
}
