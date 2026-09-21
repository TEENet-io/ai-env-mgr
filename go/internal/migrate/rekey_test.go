package migrate

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

func keyB64(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.StdEncoding.EncodeToString(raw)
}

func keyFile(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRekeyResealsEverythingLiveAndLeavesRetiredRows(t *testing.T) {
	store, ctx := newStore(t)
	dir := t.TempDir()
	k1, k2 := keyB64(t), keyB64(t)
	oldRing, err := secrets.NewFileKeyring(keyFile(t, dir, fmt.Sprintf(`{"current":"k1","keys":{"k1":%q}}`, k1)))
	if err != nil {
		t.Fatal(err)
	}
	seal := func(ring secrets.Keyring, plain, aad string) repo.Sealed {
		blob, v, err := ring.Seal(ctx, []byte(plain), aad)
		if err != nil {
			t.Fatal(err)
		}
		return repo.Sealed{Ciphertext: blob, KeyVersion: v}
	}

	// Under k1: a live credential, a retired one, an administrator's seed,
	// and the SMTP password in the channel settings.
	e, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	aad := secrets.AAD("credential_versions", e.ID, repo.PurposeCodexGateway)
	retiredSealed := seal(oldRing, "old-token", aad)
	retired, _ := store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: 1, Purpose: repo.PurposeCodexGateway, Ciphertext: retiredSealed.Ciphertext, KeyVersion: retiredSealed.KeyVersion})
	liveSealed := seal(oldRing, "live-token", aad)
	live, _ := store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: 2, Purpose: repo.PurposeCodexGateway, Ciphertext: liveSealed.Ciphertext, KeyVersion: liveSealed.KeyVersion})
	admin, err := store.Admins().Create(ctx, repo.NewAdmin{Username: "root", PasswordHash: "x", Role: repo.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	seed := seal(oldRing, "JBSWY3DPEHPK3PXP", authn.TOTPAAD(admin.ID))
	if err := store.Admins().SetTOTP(ctx, admin.ID, seed.Ciphertext, seed.KeyVersion, []string{"h1"}); err != nil {
		t.Fatal(err)
	}
	channels := repo.ChannelSettings{}
	channels.SMTP.Host, channels.SMTP.Password = "smtp.example.com", seal(oldRing, "hunter2", repo.ChannelSecretAAD("smtp_password"))
	value, _ := json.Marshal(channels)
	store.Settings().Set(ctx, repo.SettingAlertChannels, value, 0, "test")

	// The operator adds k2 as current and runs rekey.
	path := keyFile(t, dir, fmt.Sprintf(`{"current":"k2","keys":{"k1":%q,"k2":%q}}`, k1, k2))
	newRing, _ := secrets.NewFileKeyring(path)
	dry, err := Rekey(ctx, store, newRing, true)
	if err != nil || dry.Credentials != 1 || dry.Admins != 1 || dry.Settings != 1 {
		t.Fatalf("dry run = %+v %v", dry, err)
	}
	if c, _ := store.Credentials().ByID(ctx, live.ID); c.KeyVersion != "k1" {
		t.Fatal("a dry run changed a row")
	}
	rep, err := Rekey(ctx, store, newRing, false)
	if err != nil || rep.Credentials != 1 || rep.Admins != 1 || rep.Settings != 1 {
		t.Fatalf("rekey = %+v %v", rep, err)
	}
	if fmt.Sprint(rep.Referenced) != "[k1 k2]" {
		t.Fatalf("referenced = %v (the retired row still names k1)", rep.Referenced)
	}
	c, _ := store.Credentials().ByID(ctx, live.ID)
	if c.KeyVersion != "k2" {
		t.Fatalf("live credential under %s", c.KeyVersion)
	}
	if got, err := newRing.Open(ctx, c.Ciphertext, c.KeyVersion, aad); err != nil || string(got) != "live-token" {
		t.Fatalf("the moved credential opens to %q, %v", got, err)
	}
	if r, _ := store.Credentials().ByID(ctx, retired.ID); r.KeyVersion != "k1" {
		t.Fatal("a retired row must be left alone")
	}
	a, _ := store.Admins().ByID(ctx, admin.ID)
	if a.TOTPKeyVersion != "k2" {
		t.Fatalf("seed under %s", a.TOTPKeyVersion)
	}
	if got, err := newRing.Open(ctx, a.TOTPSecret, a.TOTPKeyVersion, authn.TOTPAAD(admin.ID)); err != nil || string(got) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("the moved seed opens to %q, %v", got, err)
	}
	ch, _, _ := repo.LoadChannelSettings(ctx, store.Settings())
	if ch.SMTP.Password.KeyVersion != "k2" || ch.SMTP.Host != "smtp.example.com" {
		t.Fatalf("channel settings after rekey: %+v", ch)
	}
	if got, err := newRing.Open(ctx, ch.SMTP.Password.Ciphertext, "k2", repo.ChannelSecretAAD("smtp_password")); err != nil || string(got) != "hunter2" {
		t.Fatalf("the moved password opens to %q, %v", got, err)
	}
	// Running again finds nothing to do.
	if again, _ := Rekey(ctx, store, newRing, false); again.Credentials+again.Admins+again.Settings != 0 {
		t.Fatalf("second run moved %+v", again)
	}
	// k1 is still referenced by the retired row: pruning keeps it.
	dropped, err := PruneKeyFile(path, rep.Referenced)
	if err != nil || len(dropped) != 0 {
		t.Fatalf("prune with k1 referenced: dropped=%v err=%v", dropped, err)
	}
	// Once nothing references k1 (say the retired rows were purged), it goes.
	dropped, err = PruneKeyFile(path, []string{"k2"})
	if err != nil || fmt.Sprint(dropped) != "[k1]" {
		t.Fatalf("prune: dropped=%v err=%v", dropped, err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatal("no backup written")
	}
	pruned, err := secrets.NewFileKeyring(path)
	if err != nil || pruned.CurrentVersion() != "k2" {
		t.Fatalf("pruned file: %v", err)
	}
	if _, err := pruned.Open(ctx, retiredSealed.Ciphertext, "k1", aad); err == nil {
		t.Fatal("k1 is still in the file")
	}
	// A referenced version missing from the file is refused loudly.
	if _, err := PruneKeyFile(path, []string{"k1", "k2"}); err == nil {
		t.Fatal("a referenced version missing from the file must be an error")
	}
}
