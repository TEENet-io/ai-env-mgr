package admincore

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/creds"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func TestReissueRejectsAllRetiredModelsWithoutMutation(t *testing.T) {
	m, store, gw := onboarded(t)
	u := gw.users["emp-alice"]
	u.Models = []string{"retired-model"}
	gw.users["emp-alice"] = u
	before := append([]byte(nil), store.objects[credsKey("alice")]...)
	generated, deleted := len(gw.generated), len(gw.deleted)
	err := m.Reissue(context.Background(), gw, testGW, "alice")
	if err == nil || !strings.Contains(err.Error(), "select") {
		t.Errorf("expected model selection error, got %v", err)
	}
	if len(gw.generated) != generated || len(gw.deleted) != deleted {
		t.Error("rejected reissue mutated tokens")
	}
	if !bytes.Equal(before, store.objects[credsKey("alice")]) {
		t.Error("rejected reissue changed credentials")
	}
	if !reflect.DeepEqual(gw.users["emp-alice"].Models, u.Models) {
		t.Error("rejected reissue changed user permissions")
	}
}

func TestPublishCredentialsPreservesArchiveOnReadFailure(t *testing.T) {
	for _, fault := range []string{"timeout", "403", "corrupt zip"} {
		t.Run(fault, func(t *testing.T) {
			m, store, _ := onboarded(t)
			key := credsKey("alice")
			if fault == "corrupt zip" {
				store.objects[key] = []byte("invalid zip")
			} else {
				store.getErrFor = key
				store.getErr = errors.New(fault)
			}
			before := append([]byte(nil), store.objects[key]...)
			err := m.PublishCredentials("alice", model.CredentialSet{model.PathCodexModels: []byte(`{"models":[]}`)})
			if err == nil {
				t.Error("must report existing archive failure")
			}
			if !bytes.Equal(before, store.objects[key]) {
				t.Error("existing archive overwritten after read failure")
			}
		})
	}
}

func TestSetModelsFailureStagesAndRetry(t *testing.T) {
	for _, stage := range []string{"user", "token", "catalog"} {
		t.Run(stage, func(t *testing.T) {
			m, store, gw := onboarded(t)
			before := append([]byte(nil), store.objects[credsKey("alice")]...)
			switch stage {
			case "user":
				gw.upsertErr = errors.New("injected user failure")
			case "token":
				gw.updateErr = errors.New("injected token failure")
			case "catalog":
				store.putErrFor = credsKey("alice")
			}
			desired := []string{"grok-4.6"}
			err := m.SetModels(context.Background(), gw, testGW, "alice", desired)
			if err == nil || !strings.Contains(err.Error(), stage) {
				t.Fatalf("expected %s stage error, got %v", stage, err)
			}
			if !bytes.Equal(before, store.objects[credsKey("alice")]) {
				t.Error("failed operation changed delivered archive")
			}
			wantUser, wantKey := []string{"glm-5"}, []string{"glm-5"}
			if stage != "user" {
				wantUser = desired
			}
			if stage == "catalog" {
				wantKey = desired
			}
			if !reflect.DeepEqual(gw.users["emp-alice"].Models, wantUser) {
				t.Errorf("unexpected user permissions: %v", gw.users["emp-alice"].Models)
			}
			if !reflect.DeepEqual(gw.existing["emp-alice"].Models, wantKey) {
				t.Errorf("unexpected key permissions: %v", gw.existing["emp-alice"].Models)
			}
			entries, _ := m.ReadAudit("alice")
			if entries[0].Action != AuditModels || entries[0].Detail["failed"] != true {
				t.Errorf("failed change missing audit: %+v", entries[0])
			}
			gw.upsertErr = nil
			gw.updateErr = nil
			store.putErrFor = ""
			if err := m.SetModels(context.Background(), gw, testGW, "alice", desired); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gw.users["emp-alice"].Models, desired) || !reflect.DeepEqual(gw.existing["emp-alice"].Models, desired) {
				t.Error("retry did not converge permissions")
			}
			set := deliveredSet(t, store, "alice")
			if !strings.Contains(string(set[model.PathCodexModels]), `"slug": "grok-4.6"`) || strings.Contains(string(set[model.PathCodexModels]), `"slug": "glm-5"`) {
				t.Error("retry did not converge catalog")
			}
			if !bytes.Equal(set[model.PathCodexConfig], deliveredConfig(t, before)) {
				t.Error("model change replaced existing config/token")
			}
			if len(gw.generated) != 1 || len(gw.deleted) != 0 {
				t.Error("model update rotated tokens")
			}
		})
	}
}

func deliveredConfig(t *testing.T, blob []byte) []byte {
	t.Helper()
	s, err := creds.Unpack(blob)
	if err != nil {
		t.Fatal(err)
	}
	return s[model.PathCodexConfig]
}
