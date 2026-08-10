// Command admin is the administrator CLI for the AI sandbox.
//
// It signs in on an employee's behalf, pushes the website block policy to
// every machine, and reports the fleet's health.
package main

import (
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

var version = "dev"

// cachedCreds holds the OSS settings resolved once, so the TUI (and any run
// that issues several commands) prompts for the AccessKey a single time and
// reuses it instead of asking again for every action.
var cachedCreds *config.Config

func newManager() (*admincore.Manager, error) {
	cfg := cachedCreds
	if cfg == nil {
		c, _, err := resolveAdminCreds()
		if err != nil {
			return nil, err
		}
		cfg = c
	}
	store, err := ossclient.New(cfg.Endpoint, cfg.Bucket, cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, err
	}
	return &admincore.Manager{Store: store}, nil
}

func main() {
	if len(os.Args) < 2 {
		// No command on a terminal: drop into the menu-driven TUI. Piped/
		// non-interactive runs still get the usage text instead of a menu that
		// could never be answered.
		if term.IsTerminal(int(os.Stdin.Fd())) {
			if err := runTUI(); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
		usage()
		return
	}

	var err error
	switch os.Args[1] {
	case "tui":
		err = runTUI()
	case "login":
		err = cmdLogin(os.Args[2:])
	case "user":
		err = cmdUser(os.Args[2:])
	case "machine":
		err = cmdMachine(os.Args[2:])
	case "file":
		err = cmdFile(os.Args[2:])
	case "add-site":
		err = cmdAddSite(os.Args[2:])
	case "remove-site":
		err = cmdRemoveSite(os.Args[2:])
	case "list-sites":
		err = cmdListSites()
	case "enable-block":
		err = cmdSetBlock(true)
	case "disable-block":
		err = cmdSetBlock(false)
	case "set-interval":
		err = cmdSetInterval(os.Args[2:])
	case "collect":
		err = cmdCollect(os.Args[2:])
	case "status":
		err = cmdStatus()
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`admin.exe -- AI Env Mgr administration

Accounts
  login --user <name> --tool <codex|claude|all>   sign in on a user's behalf
  user add <name> [--codex <email>] [--claude <email>]
  user disable <name>
  user enable <name>
  user list

Machines
  machine bind <hostname> --user <name> [--note <text>]
  machine unbind <hostname>
  machine list

File transfer
  file put <path> [--as <name>] [--expires <hours>]   stage a file and print
                                                      a download link
  file link <name> [--expires <hours>]                reissue a link
  file list                                           show what is staged
  file rm <name>                                      remove a staged file

Website blocking
  add-site <domain>...        block more domains
  remove-site <domain>...     unblock domains
  list-sites                  show the current block list
  enable-block                restore blocking everywhere
  disable-block               lift blocking everywhere (temporary access)

Agents
  set-interval <minutes>      change how often agents sync
  status                      show every machine's state

Session collection (off by default)
  collect enable [--since <YYYY-MM-DD>] [--quiet <seconds>]   turn collection on
  collect disable                                             turn collection off
  collect stat                                                per-employee upload counts

Other
  tui                         interactive menu (also the default when run with
                              no command on a terminal)
  version
`)
}
