// Command admin serves the administrator console for the AI sandbox.
//
// Everything is done from the browser: the roster, machine bindings, website
// blocking, signing in on an employee's behalf, and publishing agent or Codex
// builds to the fleet. The terminal interface this program used to carry was
// removed once the console covered all of it -- two front ends over the same
// operations kept drifting apart, and each divergence was a bug nobody saw
// until somebody hit it.
//
// There is no separate server to keep running for occasional use: with no
// arguments it listens on localhost, so the console can be opened on the
// administrator's own machine exactly like the old menu.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		// Bare `admin` opens the console locally, which is what the menu used
		// to do and keeps the fleet reachable when the deployed console is not.
		args = []string{"web"}
	}

	var err error
	switch args[0] {
	case "web":
		err = cmdWeb(args[1:])
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
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
	fmt.Print(`admin -- AI Env Mgr administration console

Run with no arguments to open the console on this machine:

  admin                       serve on 127.0.0.1:8080, then open it in a browser

  web [--listen <host:port>] [--cert <file> --key <file>] [--behind-proxy]
                              serve the console. Sign in with the OSS
                              AccessKey; it is kept in memory only, never on
                              disk. TLS is required unless bound to 127.0.0.1,
                              or to a private address with --behind-proxy.
  version

Everything else -- employees, machines, website blocking, signing in for an
employee, publishing agent and Codex builds -- is in the console itself.
`)
}
