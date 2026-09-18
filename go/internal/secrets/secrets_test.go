package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKeyFile puts a master key file on disk with the given mode and returns
// its path.
func writeKeyFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	// WriteFile is subject to the umask; make the mode exact.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return path
}

func randomKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, keySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func oneKeyFile(t *testing.T) string {
	t.Helper()
	return writeKeyFile(t, fmt.Sprintf(`{"current":"k1","keys":{"k1":%q}}`, randomKey(t)), 0o600)
}

func TestSealAndOpenRoundTrip(t *testing.T) {
	ring, err := NewFileKeyring(oneKeyFile(t))
	if err != nil {
		t.Fatalf("NewFileKeyring: %v", err)
	}
	ctx := context.Background()
	secret := []byte("sk-gateway-token-0123456789")
	aad := AAD("credential_versions", "5f1c", "codex_gateway")

	blob, version, err := ring.Seal(ctx, secret, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if version != "k1" {
		t.Errorf("key version = %q, want k1", version)
	}
	if bytes.Contains(blob, secret) {
		t.Fatal("the plaintext is sitting in the ciphertext")
	}

	got, err := ring.Open(ctx, blob, version, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("Open returned %q, want %q", got, secret)
	}
}

func TestSealingTheSameValueTwiceDiffers(t *testing.T) {
	ring, err := NewFileKeyring(oneKeyFile(t))
	if err != nil {
		t.Fatalf("NewFileKeyring: %v", err)
	}
	ctx := context.Background()
	aad := AAD("credential_versions", "5f1c", "codex_gateway")

	first, _, err := ring.Seal(ctx, []byte("same token"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, _, err := ring.Seal(ctx, []byte("same token"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Equal ciphertexts would publish "these two rows hold the same secret"
	// to anyone who can read the table.
	if bytes.Equal(first, second) {
		t.Error("two seals of the same value produced identical ciphertext")
	}
}

func TestOpenRejectsADifferentRow(t *testing.T) {
	ring, err := NewFileKeyring(oneKeyFile(t))
	if err != nil {
		t.Fatalf("NewFileKeyring: %v", err)
	}
	ctx := context.Background()
	blob, version, err := ring.Seal(ctx, []byte("employee A's token"),
		AAD("credential_versions", "row-a", "codex_gateway"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Moving the blob into another row must not open it: otherwise "read
	// employee A's token" is one UPDATE away from "read it as employee B".
	for _, aad := range []string{
		AAD("credential_versions", "row-b", "codex_gateway"),
		AAD("credential_versions", "row-a", "totp_seed"),
		AAD("admin_principals", "row-a", "codex_gateway"),
		"",
	} {
		if _, err := ring.Open(ctx, blob, version, aad); !errors.Is(err, ErrWrongKey) {
			t.Errorf("Open with aad %q: error = %v, want ErrWrongKey", aad, err)
		}
	}
}

func TestOpenRejectsTamperedBytes(t *testing.T) {
	ring, err := NewFileKeyring(oneKeyFile(t))
	if err != nil {
		t.Fatalf("NewFileKeyring: %v", err)
	}
	ctx := context.Background()
	aad := AAD("credential_versions", "5f1c", "codex_gateway")
	blob, version, err := ring.Seal(ctx, []byte("token"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	for _, at := range []int{len(magic), len(magic) + nonceSize + 1, len(blob) - 1} {
		tampered := bytes.Clone(blob)
		tampered[at] ^= 0x01
		if _, err := ring.Open(ctx, tampered, version, aad); !errors.Is(err, ErrWrongKey) {
			t.Errorf("Open with byte %d flipped: error = %v, want ErrWrongKey", at, err)
		}
	}
}

func TestOpenRejectsAnUnknownKeyVersion(t *testing.T) {
	ring, err := NewFileKeyring(oneKeyFile(t))
	if err != nil {
		t.Fatalf("NewFileKeyring: %v", err)
	}
	ctx := context.Background()
	aad := AAD("credential_versions", "5f1c", "codex_gateway")
	blob, _, err := ring.Seal(ctx, []byte("token"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	_, err = ring.Open(ctx, blob, "k9", aad)
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("Open with an unknown version: error = %v, want ErrWrongKey", err)
	}
	// The message should say which version is missing: the answer is nearly
	// always that this host's key file predates the row.
	if !strings.Contains(err.Error(), "k9") {
		t.Errorf("error %q does not name the missing key version", err)
	}
}

func TestOpenRejectsSomethingThatIsNotASecret(t *testing.T) {
	ring, err := NewFileKeyring(oneKeyFile(t))
	if err != nil {
		t.Fatalf("NewFileKeyring: %v", err)
	}
	ctx := context.Background()
	// A column that still holds plaintext, or a truncated copy, should say so
	// rather than produce "message authentication failed".
	for _, blob := range [][]byte{nil, []byte("plain text token"), []byte(magic + "short")} {
		_, err := ring.Open(ctx, blob, "k1", "any")
		if err == nil {
			t.Fatalf("Open(%q) succeeded", blob)
		}
		if !strings.Contains(err.Error(), "not a sealed secret") {
			t.Errorf("Open(%q) error = %v, want it to say the value is not a sealed secret", blob, err)
		}
	}
}

// A rotation changes which key new values use. Values sealed with the previous
// key must keep opening -- the alternative is every stored secret becoming
// unreadable the moment somebody rotates.
func TestRotationKeepsOldValuesReadable(t *testing.T) {
	ctx := context.Background()
	k1 := randomKey(t)
	oldRing, err := NewFileKeyring(writeKeyFile(t,
		fmt.Sprintf(`{"current":"k1","keys":{"k1":%q}}`, k1), 0o600))
	if err != nil {
		t.Fatalf("NewFileKeyring: %v", err)
	}
	aad := AAD("credential_versions", "5f1c", "codex_gateway")
	oldBlob, oldVersion, err := oldRing.Seal(ctx, []byte("issued before the rotation"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	newRing, err := NewFileKeyring(writeKeyFile(t,
		fmt.Sprintf(`{"current":"k2","keys":{"k1":%q,"k2":%q}}`, k1, randomKey(t)), 0o600))
	if err != nil {
		t.Fatalf("NewFileKeyring after rotation: %v", err)
	}
	if newRing.CurrentVersion() != "k2" {
		t.Errorf("CurrentVersion = %q, want k2", newRing.CurrentVersion())
	}
	got, err := newRing.Open(ctx, oldBlob, oldVersion, aad)
	if err != nil {
		t.Fatalf("a value sealed before the rotation no longer opens: %v", err)
	}
	if string(got) != "issued before the rotation" {
		t.Errorf("Open returned %q", got)
	}
	_, version, err := newRing.Seal(ctx, []byte("issued after"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if version != "k2" {
		t.Errorf("new values sealed with %q, want the rotated-in key k2", version)
	}
}

func TestNewFileKeyringRefusesAReadableFile(t *testing.T) {
	body := fmt.Sprintf(`{"current":"k1","keys":{"k1":%q}}`, randomKey(t))
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		_, err := NewFileKeyring(writeKeyFile(t, body, mode))
		if err == nil {
			t.Errorf("mode %04o was accepted; a master key others can read is not a master key", mode)
			continue
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: error = %v, want it to say what to do about it", mode, err)
		}
	}
	if _, err := NewFileKeyring(writeKeyFile(t, body, 0o600)); err != nil {
		t.Errorf("mode 0600 was rejected: %v", err)
	}
	if _, err := NewFileKeyring(writeKeyFile(t, body, 0o400)); err != nil {
		t.Errorf("mode 0400 was rejected: %v", err)
	}
}

func TestNewFileKeyringRejectsBadFiles(t *testing.T) {
	good := randomKey(t)
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `k1 = "abc"`, "expected JSON"},
		{"no current", fmt.Sprintf(`{"keys":{"k1":%q}}`, good), `no "current"`},
		{"no keys", `{"current":"k1","keys":{}}`, "no keys"},
		{"current missing from keys", fmt.Sprintf(`{"current":"k2","keys":{"k1":%q}}`, good), "no such key"},
		{"not base64", `{"current":"k1","keys":{"k1":"not base64!!"}}`, "valid base64"},
		{"wrong length", fmt.Sprintf(`{"current":"k1","keys":{"k1":%q}}`, short), "must be 32"},
		{"empty version", fmt.Sprintf(`{"current":"k1","keys":{"":%q,"k1":%q}}`, good, good), "empty version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewFileKeyring(writeKeyFile(t, c.body, 0o600))
			if err == nil {
				t.Fatalf("accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}

	if _, err := NewFileKeyring(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing master key file was accepted")
	}
	if _, err := NewFileKeyring(t.TempDir()); err == nil {
		t.Error("a directory was accepted as a master key file")
	}
}
