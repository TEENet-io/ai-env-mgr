package ops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

func testKeyring(t *testing.T) secrets.Keyring {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(`{"current":"k1","keys":{"k1":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = os.Chmod(path, 0o600)
	ring, err := secrets.NewFileKeyring(path)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}

func TestClaudeCredentialsAreSealedAndRetiredWithTheAccount(t *testing.T) {
	svc, store, ctx := newService(t)
	ring := testKeyring(t)
	employee, err := svc.Onboard(ctx, OnboardSpec{WindowsUser: "work1", Quota: testQuota(), Actor: "zhang"})
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	finish(t, ctx, store)

	set := model.CredentialSet{
		model.PathClaudeCreds:  []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-secret"}}`),
		model.PathClaudeConfig: []byte(`{"hasCompletedOnboarding":true}`),
		model.PathCodexConfig:  []byte(`should not be taken`),
	}
	if err := svc.PublishClaudeCredentials(ctx, ring, employee.ID, set, "zhang", ""); err != nil {
		t.Fatalf("publish: %v", err)
	}

	credential, err := store.Credentials().Live(ctx, employee.ID, repo.PurposeClaudeLogin)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if strings.Contains(string(credential.Ciphertext), "sk-ant-secret") {
		t.Fatal("the Claude token is stored in the clear")
	}
	plain, err := ring.Open(ctx, credential.Ciphertext, credential.KeyVersion,
		secrets.AAD("credential_versions", employee.ID, repo.PurposeClaudeLogin))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if strings.Contains(string(plain), "should not be taken") {
		t.Error("a Codex file was stored under the Claude purpose")
	}
	if len(openTasks(t, ctx, store)) != 1 {
		t.Errorf("queued %v, want one export", kinds(openTasks(t, ctx, store)))
	}
	// The audit row names the files and never their contents.
	history, _ := store.Audit().ByTarget(ctx, "employee", employee.ID, 1)
	if len(history) != 1 || strings.Contains(string(history[0].After), "sk-ant") {
		t.Errorf("audit = %+v", history)
	}

	if _, err := svc.Offboard(ctx, employee.ID, "zhang", ""); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if _, err := store.Credentials().Live(ctx, employee.ID, repo.PurposeClaudeLogin); !errors.Is(err, repo.ErrNotFound) {
		t.Error("the Claude credentials survived offboarding")
	}
	if err := svc.PublishClaudeCredentials(ctx, ring, employee.ID, set, "zhang", ""); err == nil {
		t.Error("credentials were published for a closed account")
	}
}
