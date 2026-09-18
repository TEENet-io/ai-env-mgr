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
	addTestUser(t, m, "work1", "")

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

// A partial publish must not erase the entries published earlier for the
// same user.
func TestPublishCredentialsMergesWithExisting(t *testing.T) {
	m, store := newManager()
	addTestUser(t, m, "work1", "")

	codex := model.CredentialSet{model.PathCodexAuth: []byte("codex-token")}
	if err := m.PublishCredentials("work1", codex); err != nil {
		t.Fatal(err)
	}

	catalog := model.CredentialSet{model.PathCodexModels: []byte(`{"models":[]}`)}
	if err := m.PublishCredentials("work1", catalog); err != nil {
		t.Fatal(err)
	}

	blob := store.objects[ossclient.UserKey("work1", "credentials.zip")]
	got, err := creds.Unpack(blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[model.PathCodexAuth]) != "codex-token" {
		t.Error("publishing the catalog dropped the existing auth entry")
	}
	if string(got[model.PathCodexModels]) != `{"models":[]}` {
		t.Error("the catalog was not published")
	}
}
