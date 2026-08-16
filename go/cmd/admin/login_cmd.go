package main

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/authflow"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// loginTimeout bounds the whole sign-in, including the time the
// administrator spends in the browser. It only limits the token exchange
// once a code has been pasted; the paste itself blocks on stdin.
const loginTimeout = 10 * time.Minute

func cmdLogin(args []string) error {
	user, tool := "", "all"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--user":
			if i+1 >= len(args) {
				return fmt.Errorf("--user needs a value")
			}
			user = args[i+1]
			i++
		case "--tool":
			if i+1 >= len(args) {
				return fmt.Errorf("--tool needs a value")
			}
			tool = args[i+1]
			i++
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if user == "" {
		return fmt.Errorf("usage: admin login --user <name> --tool <codex|claude|all>")
	}
	switch tool {
	case "codex", "claude", "all":
	default:
		return fmt.Errorf("--tool must be codex, claude or all (got %q)", tool)
	}

	mgr, err := newManager()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()

	if tool == "codex" || tool == "all" {
		if err := loginAndPublish(ctx, mgr, user, "Codex", loginCodex); err != nil {
			return err
		}
	}
	if tool == "claude" || tool == "all" {
		if err := loginAndPublish(ctx, mgr, user, "Claude", loginClaude); err != nil {
			return err
		}
	}

	fmt.Println("\nagents will deliver these on their next sync")
	return nil
}

// loginAndPublish runs one tool's flow and uploads the result.
//
// Tokens are assembled in memory and handed straight to the store, so the
// administrator's own disk never holds an employee's credentials.
func loginAndPublish(ctx context.Context, mgr *admincore.Manager, user, label string,
	flow func(context.Context) (model.CredentialSet, error)) error {

	fmt.Printf("\n=== %s login for %s ===\n", label, user)
	set, err := flow(ctx)
	if err != nil {
		return fmt.Errorf("%s login: %w", strings.ToLower(label), err)
	}
	if err := mgr.PublishCredentials(user, set); err != nil {
		return err
	}
	fmt.Printf("[ok] %s credentials published for %s\n", label, user)
	return nil
}

// loginCodex runs the Codex flow. Codex redirects to localhost, so the code
// The administrator pastes the callback URL back, the same way the validated
// PowerShell prototype did.
func loginCodex(ctx context.Context) (model.CredentialSet, error) {
	verifier, challenge, state, err := authflow.NewCodexPKCE()
	if err != nil {
		return nil, err
	}

	fmt.Println("\nOpen this URL in a browser and sign in with the employee's Codex account:")
	fmt.Println()
	fmt.Println(authflow.CodexAuthorizeURL(challenge, state))
	fmt.Println()
	fmt.Println("After signing in the browser will try to open")
	fmt.Println("  http://localhost:1455/auth/callback?code=...&state=...")
	fmt.Println("and show a connection error. That is expected -- the address bar still")
	fmt.Println("holds the code. Copy the WHOLE URL from the address bar and paste it here:")
	fmt.Print("> ")

	pasted, err := readPasted()
	if err != nil {
		return nil, err
	}

	code, err := codeFromCallback(pasted, state)
	if err != nil {
		return nil, err
	}
	fmt.Println("[ok] authorization code captured")

	tokens, err := authflow.CodexExchange(ctx, code, verifier)
	if err != nil {
		return nil, err
	}
	authJSON, err := authflow.BuildCodexAuthJSON(tokens)
	if err != nil {
		return nil, err
	}
	return model.CredentialSet{model.PathCodexAuth: authJSON}, nil
}

// readPasted reads one line from the terminal.
func readPasted() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read the pasted value: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("nothing was pasted")
	}
	return line, nil
}

// codeFromCallback pulls the authorization code out of whatever the
// administrator pasted, accepting either the full callback URL or the bare
// code, and refuses a callback belonging to a different login.
//
// The code is taken by pasting rather than by listening on localhost:1455
// because the browser is usually not on the machine running this command --
// the administrator drives a fleet of cloud desktops from their own laptop.
// A listener would also collide with Codex itself, which uses that port.
func codeFromCallback(pasted, wantState string) (string, error) {
	// An empty wantState would compare equal to an absent one and let the
	// check pass, so refuse rather than match.
	if wantState == "" {
		return "", fmt.Errorf("this login is missing its state; start again")
	}
	if u, err := url.Parse(pasted); err == nil && u.Query().Get("code") != "" {
		// The state must match, and a callback carrying none does not.
		// Letting an absent state through would accept a code this login
		// never asked for -- paste a crafted URL and somebody else's tokens
		// get published as the employee's credentials. PKCE already makes
		// that hard, since a code issued for another challenge will not
		// exchange against our verifier; this is the check that says so.
		if u.Query().Get("state") != wantState {
			return "", fmt.Errorf("that callback belongs to a different login attempt (state mismatch); start again")
		}
		return u.Query().Get("code"), nil
	}

	if strings.Contains(pasted, "code=") {
		return "", fmt.Errorf("could not parse that URL; paste the whole address bar contents")
	}
	return "", fmt.Errorf("paste the whole callback URL, not just the code -- the state parameter in it is what ties the code to this login")
}

// loginClaude runs the Claude flow. Claude shows the code on its own page
// instead of redirecting to localhost, so the administrator pastes it back.
func loginClaude(ctx context.Context) (model.CredentialSet, error) {
	verifier, challenge, state, err := authflow.NewClaudePKCE()
	if err != nil {
		return nil, err
	}

	fmt.Println("\nOpen this URL in a browser and sign in with the employee's Claude account:")
	fmt.Println()
	fmt.Println(authflow.ClaudeAuthorizeURL(challenge, state))
	fmt.Println()
	fmt.Println("Claude then shows an authorization code. Paste it here:")
	fmt.Print("> ")

	pasted, err := readPasted()
	if err != nil {
		return nil, err
	}
	// Be forgiving if a whole callback URL gets pasted instead of just the code.
	if u, perr := url.Parse(pasted); perr == nil && u.Query().Get("code") != "" {
		pasted = u.Query().Get("code")
	}

	code, gotState := authflow.ParsePastedClaudeCode(pasted, state)
	tokens, err := authflow.ClaudeExchange(ctx, code, gotState, verifier)
	if err != nil {
		return nil, err
	}
	credsJSON, err := authflow.BuildClaudeCredentialsJSON(tokens)
	if err != nil {
		return nil, err
	}

	// Claude Code reads its signed-in identity from ~/.claude.json. Without
	// it the tokens are on disk but the employee is still shown the
	// onboarding wizard and asked to log in.
	configJSON, err := authflow.BuildClaudeConfigJSON(tokens)
	if err != nil {
		return nil, err
	}
	if !tokens.HasAccountIdentity() {
		fmt.Println("warning: the token response did not identify the account, so")
		fmt.Println("         claude.json carries no oauthAccount. If the employee is")
		fmt.Println("         still asked to sign in, this is why -- report it.")
	}

	return model.CredentialSet{
		model.PathClaudeCreds:  credsJSON,
		model.PathClaudeConfig: configJSON,
	}, nil
}
