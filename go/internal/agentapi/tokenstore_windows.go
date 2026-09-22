//go:build windows

package agentapi

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The token is sealed with DPAPI in the machine scope: the service runs as
// LocalSystem and must be able to read it after any restart, and a copy of
// the file on another machine must be useless.
const cryptProtectLocalMachine = 0x4

var tokenEntropy = []byte("ai-env-mgr device token v1")

func protect(plain []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	entropy := windows.DataBlob{Size: uint32(len(tokenEntropy)), Data: &tokenEntropy[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, &entropy, 0, nil, cryptProtectLocalMachine, &out); err != nil {
		return nil, fmt.Errorf("seal device token: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func unprotect(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, ErrNoToken
	}
	in := windows.DataBlob{Size: uint32(len(sealed)), Data: &sealed[0]}
	entropy := windows.DataBlob{Size: uint32(len(tokenEntropy)), Data: &tokenEntropy[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, &entropy, 0, nil, cryptProtectLocalMachine, &out); err != nil {
		return nil, fmt.Errorf("open device token: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
