package agentcore

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func markerPath(s *Syncer) string {
	return filepath.Join(s.StateDir, credsMarkerFile)
}

func TestLegacyBareETagMarkerForcesRedelivery(t *testing.T) {
	// This is the failure that motivated the manifest. An older agent had no
	// target for a newly introduced archive entry, wrote what it knew, and
	// recorded the bare ETag. After the upgrade the ETag still matched, so the
	// archive was never fetched again and the missing file stayed missing --
	// with the marker reporting success the whole time.
	s := &Syncer{StateDir: t.TempDir()}
	if err := os.WriteFile(markerPath(s), []byte("some-etag"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.readCredsMark(); ok {
		t.Fatal("a marker with no manifest must not be trusted; the archive has to be fetched again")
	}
}

func TestMarkRoundTripsAndVerifies(t *testing.T) {
	dir := t.TempDir()
	s := &Syncer{StateDir: dir}

	target := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(target, []byte("model = \"grok-4.6\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// SHA-256 of that exact content.
	placed := map[string]string{target: sha256Hex(t, target)}

	s.writeCredsMark("etag-1", placed)
	m, ok := s.readCredsMark()
	if !ok || m.ETag != "etag-1" || len(m.Placed) != 1 {
		t.Fatalf("marker did not round-trip: %+v ok=%v", m, ok)
	}
	if !(&Syncer{StateDir: dir}).credsIntact(m) {
		t.Error("an untouched file failed verification")
	}

	// The employee edits the delivered file: the next cycle must notice and
	// redeliver rather than trust the ETag.
	if err := os.WriteFile(target, []byte("model = \"something-else\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if (&Syncer{StateDir: dir}).credsIntact(m) {
		t.Error("an edited file passed verification")
	}

	// And a deleted one.
	os.Remove(target)
	if (&Syncer{StateDir: dir}).credsIntact(m) {
		t.Error("a deleted file passed verification")
	}
}

func TestEmptyManifestIsNotWritten(t *testing.T) {
	// Writing a marker with nothing in it would recreate the old behaviour:
	// an ETag that claims delivery with no way to check it.
	s := &Syncer{StateDir: t.TempDir()}
	s.writeCredsMark("etag-1", nil)
	if _, err := os.Stat(markerPath(s)); !os.IsNotExist(err) {
		t.Error("a marker with no manifest should not have been written")
	}
}

func TestCorruptMarkerIsTreatedAsAbsent(t *testing.T) {
	s := &Syncer{StateDir: t.TempDir()}
	if err := os.WriteFile(markerPath(s), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.readCredsMark(); ok {
		t.Error("a corrupt marker must not be trusted")
	}
}

func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
