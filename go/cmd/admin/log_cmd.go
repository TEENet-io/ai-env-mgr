package main

import (
	"fmt"
	"os"
)

func cmdLog(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: admin log <hostname>")
	}
	mgr, err := newManager()
	if err != nil {
		return err
	}
	data, err := mgr.FetchLog(args[0])
	if err != nil {
		return err
	}
	os.Stdout.Write(data)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		fmt.Println()
	}
	return nil
}
