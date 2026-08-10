package admincore

import (
	"reflect"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func TestPublishCredentialsRoundTrips(t *testing.T) {
	m, store := newManager()
	if err := m.AddUser("work1", "", ""); err != nil {
		t.Fatal(err)
	}

	set := model.CredentialSet{model.PathCodexAuth: []byte(`{"token":"abc"}`)}
	if err := m.PublishCredentials("work1", set); err != nil {
		t.Fatal(err)
	}

	blob, ok := store.objects[ossclient.UserKey("work1", "credentials.zip")]
	if !ok {
		t.Fatal("credentials were not uploaded")
	}
	got, err := creds.Unpack(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, set) {
		t.Errorf("Unpack = %v, want %v", got, set)
	}
}

// A user not yet in the roster has no directory of their own; publishing to
// them would just scatter an object under an arbitrary key.
func TestPublishCredentialsRejectsUnknownUser(t *testing.T) {
	m, _ := newManager()
	set := model.CredentialSet{model.PathCodexAuth: []byte("{}")}
	if err := m.PublishCredentials("ghost", set); err == nil {
		t.Fatal("expected error for a user not in the roster")
	}
}

// Publishing Claude credentials alone must not erase Codex credentials
// published earlier for the same user.
func TestPublishCredentialsMergesWithExisting(t *testing.T) {
	m, store := newManager()
	if err := m.AddUser("work1", "", ""); err != nil {
		t.Fatal(err)
	}

	codex := model.CredentialSet{model.PathCodexAuth: []byte("codex-token")}
	if err := m.PublishCredentials("work1", codex); err != nil {
		t.Fatal(err)
	}

	claude := model.CredentialSet{model.PathClaudeCreds: []byte("claude-token")}
	if err := m.PublishCredentials("work1", claude); err != nil {
		t.Fatal(err)
	}

	blob := store.objects[ossclient.UserKey("work1", "credentials.zip")]
	got, err := creds.Unpack(blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[model.PathCodexAuth]) != "codex-token" {
		t.Error("publishing claude creds dropped the existing codex creds")
	}
	if string(got[model.PathClaudeCreds]) != "claude-token" {
		t.Error("claude creds were not published")
	}
}
