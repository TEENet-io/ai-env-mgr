package creds

import (
	"bytes"
	"testing"

	"github.com/TEENet-io/airlock/internal/model"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	in := model.CredentialSet{
		model.PathCodexAuth:   []byte(`{"auth_mode":"chatgpt"}`),
		model.PathClaudeCreds: []byte(`{"claudeAiOauth":{}}`),
	}
	blob, err := Pack(in)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("Pack produced no bytes")
	}
	out, err := Unpack(blob)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d entries, want %d", len(out), len(in))
	}
	for k, v := range in {
		if !bytes.Equal(out[k], v) {
			t.Errorf("entry %q: got %q, want %q", k, out[k], v)
		}
	}
}

// The archive is uploaded and compared by ETag, so identical input must
// produce identical bytes or every sync would look like a change.
func TestPackIsDeterministic(t *testing.T) {
	set := model.CredentialSet{
		model.PathClaudeCreds:  []byte("claude"),
		model.PathCodexAuth:    []byte("codex"),
		model.PathClaudeConfig: []byte("config"),
	}
	first, err := Pack(set)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Pack(set)
		if err != nil {
			t.Fatalf("Pack (repeat %d): %v", i, err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("archive bytes differ between runs (repeat %d)", i)
		}
	}
}

func TestPackRejectsEmptySet(t *testing.T) {
	if _, err := Pack(model.CredentialSet{}); err == nil {
		t.Error("packing an empty set should fail")
	}
	if _, err := Pack(nil); err == nil {
		t.Error("packing a nil set should fail")
	}
}

func TestUnpackRejectsGarbage(t *testing.T) {
	if _, err := Unpack([]byte("not a zip file")); err == nil {
		t.Error("unpacking garbage should fail")
	}
	if _, err := Unpack(nil); err == nil {
		t.Error("unpacking nothing should fail")
	}
}

func TestUnpackRejectsEmptyArchive(t *testing.T) {
	// A valid but empty zip carries no credentials and should be reported.
	empty, err := Pack(model.CredentialSet{"placeholder": []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	set, err := Unpack(empty)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if len(set) != 1 {
		t.Errorf("got %d entries, want 1", len(set))
	}
}

func TestMergeOverlaysWithoutLosingEntries(t *testing.T) {
	existing := model.CredentialSet{
		model.PathCodexAuth:   []byte("old codex"),
		model.PathClaudeCreds: []byte("old claude"),
	}
	incoming := model.CredentialSet{
		model.PathClaudeCreds: []byte("new claude"),
	}
	merged := Merge(existing, incoming)

	if string(merged[model.PathCodexAuth]) != "old codex" {
		t.Error("merging must not drop entries that were not republished")
	}
	if string(merged[model.PathClaudeCreds]) != "new claude" {
		t.Error("incoming entries must win over existing ones")
	}
	if len(merged) != 2 {
		t.Errorf("merged has %d entries, want 2", len(merged))
	}
}

func TestMergeDoesNotMutateInputs(t *testing.T) {
	existing := model.CredentialSet{model.PathCodexAuth: []byte("codex")}
	incoming := model.CredentialSet{model.PathClaudeCreds: []byte("claude")}
	_ = Merge(existing, incoming)

	if len(existing) != 1 || len(incoming) != 1 {
		t.Error("Merge must leave its arguments untouched")
	}
}

func TestMergeHandlesNilInputs(t *testing.T) {
	incoming := model.CredentialSet{model.PathCodexAuth: []byte("codex")}
	if merged := Merge(nil, incoming); len(merged) != 1 {
		t.Errorf("merging into nil should yield the incoming set, got %d entries", len(merged))
	}
	if merged := Merge(incoming, nil); len(merged) != 1 {
		t.Errorf("merging nil should keep the existing set, got %d entries", len(merged))
	}
}
