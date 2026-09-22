package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/backup"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// cmdBackup uploads a database dump made by the host's timer and prunes
// the old ones. The bucket credentials come from the same environment the
// console runs with; nothing is passed on the command line.
func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	file := fs.String("file", "", "the dump to upload (a .sql.gz)")
	keep := fs.Int("keep-days", 30, "dumps older than this are removed after a successful upload")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("backup: -file is required")
	}
	if info, err := os.Stat(*file); err != nil {
		return err
	} else if info.Size() == 0 {
		return errors.New("backup: the dump is empty; refusing to upload it")
	}
	built := builtIn()
	keyID, secret := os.Getenv("AIENVMGR_OSS_ACCESS_KEY_ID"), os.Getenv("AIENVMGR_OSS_ACCESS_KEY_SECRET")
	if keyID == "" || secret == "" {
		return errors.New("backup: AIENVMGR_OSS_ACCESS_KEY_ID and _SECRET must be set (source the console's environment file)")
	}
	endpoint := built.Endpoint
	if data := os.Getenv("AIENVMGR_OSS_DATA_ENDPOINT"); data != "" {
		endpoint = data
	}
	store, err := ossclient.New(endpoint, built.Bucket, keyID, secret)
	if err != nil {
		return err
	}
	key, removed, err := backup.Upload(store, *file, time.Now(), time.Duration(*keep)*24*time.Hour)
	if key != "" {
		fmt.Printf("uploaded %s\n", key)
	}
	for _, k := range removed {
		fmt.Printf("removed %s\n", k)
	}
	return err
}
