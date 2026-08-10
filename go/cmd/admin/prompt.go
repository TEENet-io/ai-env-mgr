package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// maxCredAttempts caps how many times a wrong AccessKeyId/Secret can be
// re-entered before giving up, so a script piped into a prompt cannot loop
// forever.
const maxCredAttempts = 3

// readLine reads one trimmed line; readSecret reads one value without echoing
// it. Kept as function types so promptCredentials can be tested without a
// terminal.
type readLine func(prompt string) (string, error)
type readSecret func(prompt string) (string, error)

// promptCredentials collects the OSS settings interactively and verifies them
// before returning. A definitively wrong key (ErrBadCredentials) re-prompts
// just the key pair; anything else -- a valid key with too little permission,
// a wrong bucket, a network failure -- is returned, because re-typing the same
// key would not fix it.
//
// Nothing here is written to disk: the returned Config lives only in memory
// for this one run.
func promptCredentials(out io.Writer, line readLine, secret readSecret, verify func(config.Config) error) (config.Config, error) {
	fmt.Fprintln(out, "No OSS credentials found. Enter them to continue.")
	fmt.Fprintln(out, "(Nothing is saved to disk -- you will be asked again next run.)")

	bucket, err := line("OSS bucket: ")
	if err != nil {
		return config.Config{}, err
	}
	endpoint, err := line("OSS endpoint (e.g. oss-cn-hangzhou.aliyuncs.com): ")
	if err != nil {
		return config.Config{}, err
	}

	for attempt := 1; ; attempt++ {
		keyID, err := line("AccessKeyId: ")
		if err != nil {
			return config.Config{}, err
		}
		keySecret, err := secret("AccessKeySecret: ")
		if err != nil {
			return config.Config{}, err
		}

		cfg := config.Config{
			Bucket:          bucket,
			Endpoint:        endpoint,
			AccessKeyID:     keyID,
			AccessKeySecret: keySecret,
			IntervalMinutes: config.DefaultIntervalMinutes,
		}

		switch err := verify(cfg); {
		case err == nil:
			fmt.Fprintln(out, "credentials verified.")
			return cfg, nil
		case errors.Is(err, ossclient.ErrBadCredentials):
			fmt.Fprintf(out, "  %v -- check the AccessKeyId/Secret and try again.\n", err)
			if attempt >= maxCredAttempts {
				return config.Config{}, fmt.Errorf("gave up after %d attempts: %w", attempt, err)
			}
			continue
		case errors.Is(err, ossclient.ErrAccessDenied):
			return config.Config{}, fmt.Errorf("%w -- the key is valid but its RAM policy is too narrow; widen it (see 操作手册 §2.4) and retry", err)
		default:
			return config.Config{}, fmt.Errorf("could not reach the bucket (check bucket name and endpoint): %w", err)
		}
	}
}

// resolveAdminCreds picks the admin's OSS settings: built-in first, then a
// config file, and finally an interactive prompt when neither exists and there
// is a terminal to ask on. A non-terminal (a script or CI) keeps the original
// "how to configure" error rather than blocking on input that will never come.
func resolveAdminCreds() (*config.Config, string, error) {
	if cfg, src, err := config.Resolve(builtIn(), config.DefaultAdminPath()); err == nil {
		return cfg, src, nil
	} else if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, "", fmt.Errorf("no OSS credentials: none built in, no admin.config.json, and stdin is not a terminal to prompt on (%w)", err)
	}

	in := bufio.NewReader(os.Stdin)
	// Prompts go to stderr so command output on stdout stays pipeable.
	line := func(p string) (string, error) {
		fmt.Fprint(os.Stderr, p)
		s, err := in.ReadString('\n')
		return strings.TrimSpace(s), err
	}
	secret := func(p string) (string, error) {
		fmt.Fprint(os.Stderr, p)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	verify := func(c config.Config) error {
		cl, err := ossclient.New(c.Endpoint, c.Bucket, c.AccessKeyID, c.AccessKeySecret)
		if err != nil {
			return err
		}
		return cl.Verify()
	}

	cfg, err := promptCredentials(os.Stderr, line, secret, verify)
	if err != nil {
		return nil, "", err
	}
	return &cfg, "interactive", nil
}
