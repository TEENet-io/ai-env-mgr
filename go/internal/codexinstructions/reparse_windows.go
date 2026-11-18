//go:build windows

package codexinstructions

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// rejectReparse refuses junctions and other reparse points, which Lstat
// does not report as symlinks.
func rejectReparse(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	return nil
}
