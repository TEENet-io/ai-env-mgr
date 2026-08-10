package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
)

func cmdFile(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin file <put|list|link|rm> ...")
	}
	mgr, err := newManager()
	if err != nil {
		return err
	}

	switch args[0] {
	case "put":
		return filePut(mgr, args[1:])
	case "link":
		return fileLink(mgr, args[1:])
	case "list":
		return fileList(mgr)
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: admin file rm <name>")
		}
		if err := mgr.RemoveFile(args[1]); err != nil {
			return err
		}
		fmt.Printf("removed %q\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown file subcommand %q", args[0])
	}
}

func filePut(mgr *admincore.Manager, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin file put <path> [--as <name>] [--expires <hours>]")
	}
	path := args[0]

	name := filepath.Base(path)
	ttl := time.Duration(0)
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--as":
			if i+1 >= len(args) {
				return fmt.Errorf("--as needs a value")
			}
			name = args[i+1]
			i++
		case "--expires":
			if i+1 >= len(args) {
				return fmt.Errorf("--expires needs a value in hours")
			}
			h, err := time.ParseDuration(args[i+1] + "h")
			if err != nil {
				return fmt.Errorf("--expires must be a number of hours, got %q", args[i+1])
			}
			ttl = h
			i++
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Uploading is a single request, so a very large file means one long
	// transfer with no resume. Worth saying so before the wait rather than
	// after it fails.
	const largeFile = 100 << 20
	if len(data) > largeFile {
		fmt.Fprintf(os.Stderr, "note: %s is %s; this is a single upload with no resume\n",
			name, humanSize(len(data)))
	}

	url, err := mgr.PutFile(name, data, ttl)
	if err != nil {
		return err
	}

	fmt.Printf("uploaded %s (%s)\n\n", name, humanSize(len(data)))
	printFetch(name, url, ttl)
	return nil
}

func fileLink(mgr *admincore.Manager, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: admin file link <name> [--expires <hours>]")
	}
	name := args[0]
	ttl := time.Duration(0)
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--expires":
			if i+1 >= len(args) {
				return fmt.Errorf("--expires needs a value in hours")
			}
			h, err := time.ParseDuration(args[i+1] + "h")
			if err != nil {
				return fmt.Errorf("--expires must be a number of hours, got %q", args[i+1])
			}
			ttl = h
			i++
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}

	url, err := mgr.LinkFile(name, ttl)
	if err != nil {
		return err
	}
	printFetch(name, url, ttl)
	return nil
}

func fileList(mgr *admincore.Manager) error {
	files, err := mgr.ListFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Println("nothing staged -- upload with: admin file put <path>")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tKEY")
	for _, f := range files {
		fmt.Fprintf(w, "%s\t%s\n", f.Name, f.Key)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\nissue a download link with: admin file link <name>")
	return nil
}

// printFetch shows the link plus a ready-to-paste command, because the
// receiving machine is a Windows desktop where the obvious next step is a
// PowerShell download rather than a browser.
func printFetch(name, url string, ttl time.Duration) {
	fmt.Printf("valid for %s. On the target machine:\n\n", admincoreTTL(ttl))
	fmt.Printf("  Invoke-WebRequest -Uri '%s' -OutFile '%s'\n\n", url, name)
	fmt.Println("the link carries its own authorisation, so the machine needs no credentials.")
	fmt.Println("treat it as a secret: anyone holding it can download the file until it expires.")
}

func admincoreTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return admincore.DefaultLinkTTL
	}
	if ttl > admincore.MaxLinkTTL {
		return admincore.MaxLinkTTL
	}
	return ttl
}

func humanSize(n int) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
