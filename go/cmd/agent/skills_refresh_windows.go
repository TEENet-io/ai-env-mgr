//go:build windows

package main

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// No running process means there is no in-memory cache to refresh. A desktop
// started later builds a fresh cache and also consumes the published manifest.
func codexRunningFor(user string) (bool, error) {
	sid, _, _, err := windows.LookupSID("", user)
	if errors.Is(err, windows.ERROR_NONE_MAPPED) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if strings.EqualFold(name, "ChatGPT.exe") || strings.EqualFold(name, "Codex.exe") {
			process, e := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ProcessID)
			if e != nil {
				return false, e
			}
			var token windows.Token
			e = windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token)
			windows.CloseHandle(process)
			if e != nil {
				return false, e
			}
			owner, e := token.GetTokenUser()
			token.Close()
			if e != nil {
				return false, e
			}
			if owner.User.Sid.Equals(sid) {
				return true, nil
			}
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return false, nil
	}
	return false, err
}
