// Package secrets encrypts the few values that have to be stored: the gateway
// token delivered to an employee's machine, an administrator's TOTP seed, its
// recovery codes, an alert webhook.
//
// Envelope encryption: every record gets its own random data key, the data key
// is wrapped by the master key, and both travel together in one blob. The
// master key therefore encrypts 32 bytes per record instead of the records
// themselves, which is what makes the later move to KMS a change of one type
// and nothing else -- a KMS master key never leaves the service and cannot
// encrypt a megabyte of anything, but it can wrap a data key.
//
// The master key lives in a root-owned 0600 file for now (NewFileKeyring).
// KMS comes later; the call sites talk to Keyring and will not notice.
//
// What this package does not do: it does not decide what may be decrypted, or
// by whom. Opening a value is a deliberate call at the point of use -- issuing
// credentials, verifying a TOTP code -- and plaintext never reaches a log, an
// audit row, an error message or an HTTP response.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// Keyring seals and opens values. Implementations are safe for concurrent use.
type Keyring interface {
	// Seal encrypts plaintext and reports which master key version wrapped it.
	// Store both: opening needs the version, and a rotation leaves older rows
	// pointing at older versions.
	//
	// aad binds the ciphertext to where it is stored -- "<table>:<row id>:
	// <purpose>" -- so a blob copied into another row will not open. Without
	// it, "give me employee A's token" becomes "put A's ciphertext in B's row
	// and ask for B's token".
	Seal(ctx context.Context, plaintext []byte, aad string) (ciphertext []byte, keyVersion string, err error)

	// Open reverses Seal. It fails if the blob, the key version or the aad is
	// not exactly what Seal was given.
	Open(ctx context.Context, ciphertext []byte, keyVersion, aad string) ([]byte, error)

	// CurrentVersion is the version new values are sealed with, for a status
	// page and for finding rows that still need re-wrapping after a rotation.
	CurrentVersion() string
}

// ErrWrongKey means the blob was not sealed by this key version, or the aad
// does not match, or the bytes have been changed. AES-GCM cannot tell those
// apart, and it would be a mistake to pretend otherwise: each one means the
// value in front of you is not the value that was stored.
var ErrWrongKey = errors.New("secret does not open with this key")

// Blob layout: magic, wrap nonce, wrapped data key, data nonce, sealed data.
//
// The magic is there so a value that is not a sealed blob at all -- a column
// that once held plaintext, a truncated copy -- fails with something a person
// can read instead of "message authentication failed".
const (
	magic       = "aek1"
	keySize     = 32 // AES-256
	nonceSize   = 12 // GCM standard nonce
	wrappedSize = keySize + 16
	minBlobSize = len(magic) + nonceSize + wrappedSize + nonceSize + 16
)

// fileKeyring holds master keys read from a file at startup.
type fileKeyring struct {
	current string
	keys    map[string][]byte
}

// masterKeyFile is the on-disk shape:
//
//	{"current": "k2", "keys": {"k1": "<base64 32 bytes>", "k2": "..."}}
//
// Keeping retired keys is the point of the map: a rotation changes "current"
// and new rows use the new key, while rows sealed with the old one keep
// opening until they have been re-sealed. Dropping a key is what makes a row
// unreadable forever, so it is a deliberate edit, never a side effect.
type masterKeyFile struct {
	Current string            `json:"current"`
	Keys    map[string]string `json:"keys"`
}

// NewFileKeyring loads master keys from path.
//
// It refuses a file that anyone but its owner can read. A master key readable
// by the group is not a master key; and the check has to be here, because the
// one moment somebody will not notice a wrong mode is when they are creating
// the file by hand at 2am.
//
// Create one with:
//
//	umask 077
//	printf '{"current":"k1","keys":{"k1":"%s"}}\n' "$(openssl rand -base64 32)" \
//	  > /etc/ai-env-mgr/master.key
func NewFileKeyring(path string) (Keyring, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("master key file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("master key file %s is a directory", path)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("master key file %s is mode %04o; it must not be readable by group or others (chmod 600)", path, mode)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key file: %w", err)
	}
	var f masterKeyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("master key file %s is not the expected JSON: %w", path, err)
	}
	if f.Current == "" {
		return nil, fmt.Errorf("master key file %s has no \"current\" key version", path)
	}
	if len(f.Keys) == 0 {
		return nil, fmt.Errorf("master key file %s has no keys", path)
	}

	keys := make(map[string][]byte, len(f.Keys))
	versions := make([]string, 0, len(f.Keys))
	for version := range f.Keys {
		versions = append(versions, version)
	}
	sort.Strings(versions) // deterministic error messages
	for _, version := range versions {
		if version == "" {
			return nil, fmt.Errorf("master key file %s has a key with an empty version", path)
		}
		raw, err := base64.StdEncoding.DecodeString(f.Keys[version])
		if err != nil {
			return nil, fmt.Errorf("master key %q is not valid base64: %w", version, err)
		}
		if len(raw) != keySize {
			return nil, fmt.Errorf("master key %q is %d bytes; it must be %d", version, len(raw), keySize)
		}
		keys[version] = raw
	}
	if _, ok := keys[f.Current]; !ok {
		return nil, fmt.Errorf("master key file %s names %q as current, but has no such key", path, f.Current)
	}
	return &fileKeyring{current: f.Current, keys: keys}, nil
}

func (k *fileKeyring) CurrentVersion() string { return k.current }

func (k *fileKeyring) Seal(_ context.Context, plaintext []byte, aad string) ([]byte, string, error) {
	master, ok := k.keys[k.current]
	if !ok { // unreachable: NewFileKeyring checked. Cheaper than a nil panic if that ever changes.
		return nil, "", fmt.Errorf("master key %q is not loaded", k.current)
	}

	// A fresh data key per record. Reusing one would make two ciphertexts
	// comparable -- "these two employees hold the same token" is a fact worth
	// not publishing -- and would put every record behind one nonce space.
	dataKey := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		return nil, "", fmt.Errorf("generate data key: %w", err)
	}

	wrapNonce, wrapped, err := sealWith(master, dataKey, []byte(aad))
	if err != nil {
		return nil, "", fmt.Errorf("wrap data key: %w", err)
	}
	dataNonce, sealed, err := sealWith(dataKey, plaintext, []byte(aad))
	if err != nil {
		return nil, "", fmt.Errorf("seal value: %w", err)
	}

	blob := make([]byte, 0, len(magic)+len(wrapNonce)+len(wrapped)+len(dataNonce)+len(sealed))
	blob = append(blob, magic...)
	blob = append(blob, wrapNonce...)
	blob = append(blob, wrapped...)
	blob = append(blob, dataNonce...)
	blob = append(blob, sealed...)
	return blob, k.current, nil
}

func (k *fileKeyring) Open(_ context.Context, blob []byte, keyVersion, aad string) ([]byte, error) {
	if len(blob) < minBlobSize || string(blob[:len(magic)]) != magic {
		return nil, fmt.Errorf("stored value is not a sealed secret")
	}
	master, ok := k.keys[keyVersion]
	if !ok {
		// Naming the version is safe and saves an hour: the answer is almost
		// always that the master key file on this host predates the row.
		return nil, fmt.Errorf("%w: no master key %q is loaded", ErrWrongKey, keyVersion)
	}

	rest := blob[len(magic):]
	wrapNonce, rest := rest[:nonceSize], rest[nonceSize:]
	wrapped, rest := rest[:wrappedSize], rest[wrappedSize:]
	dataNonce, sealed := rest[:nonceSize], rest[nonceSize:]

	dataKey, err := openWith(master, wrapNonce, wrapped, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: unwrapping the data key failed", ErrWrongKey)
	}
	plaintext, err := openWith(dataKey, dataNonce, sealed, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: the sealed value did not open", ErrWrongKey)
	}
	return plaintext, nil
}

func sealWith(key, plaintext, aad []byte) (nonce, sealed []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, aad), nil
}

func openWith(key, nonce, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, sealed, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return gcm, nil
}

// AAD builds the associated data for a stored secret: the table, the row and
// what the value is for. Call it rather than formatting by hand -- an aad that
// differs by a space is an aad that will not open next year.
func AAD(table, rowID, purpose string) string {
	return table + ":" + rowID + ":" + purpose
}
