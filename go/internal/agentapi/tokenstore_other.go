//go:build !windows

package agentapi

// Outside Windows there is no DPAPI; the file's 0600 mode is the whole
// protection. Only tests and development builds run here.
func protect(plain []byte) ([]byte, error)    { return plain, nil }
func unprotect(sealed []byte) ([]byte, error) { return sealed, nil }
