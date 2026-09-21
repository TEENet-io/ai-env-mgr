package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

// RekeyReport says what a re-keying did, or would do.
type RekeyReport struct {
	Current     string
	Credentials int // live credential rows moved
	Admins      int // authenticator seeds moved
	Settings    int // sealed channel secrets moved
	// Referenced is every key version some row still names after the run,
	// retired credentials included: what must stay in the key file.
	Referenced []string
}

// Rekey re-wraps every live secret under the ring's current master key.
//
// Live means what the system still opens: credentials not yet retired, the
// authenticator seeds of administrators, the channel secrets in settings.
// Retired credentials keep their old key version -- they are history, and
// the old key stays in the file until nothing references it.
//
// It is idempotent: a row already under the current key is skipped, so an
// interrupted run is finished by running again.
func Rekey(ctx context.Context, store repo.Store, ring secrets.Keyring, dryRun bool) (RekeyReport, error) {
	rep := RekeyReport{Current: ring.CurrentVersion()}

	for {
		due, err := store.Credentials().NotSealedWith(ctx, rep.Current, 100)
		if err != nil {
			return rep, err
		}
		if len(due) == 0 {
			break
		}
		if dryRun {
			rep.Credentials += len(due)
			break
		}
		err = store.InTx(ctx, func(tx repo.Store) error {
			for _, c := range due {
				blob, version, err := secrets.Reseal(ctx, ring, c.Ciphertext, c.KeyVersion,
					secrets.AAD("credential_versions", c.EmployeeID, c.Purpose))
				if err != nil {
					return fmt.Errorf("credential %s (employee %s): %w", c.ID, c.EmployeeID, err)
				}
				if err := tx.Credentials().Reseal(ctx, c.ID, blob, version); err != nil {
					return err
				}
				rep.Credentials++
			}
			return nil
		})
		if err != nil {
			return rep, err
		}
	}

	admins, err := store.Admins().List(ctx)
	if err != nil {
		return rep, err
	}
	for _, a := range admins {
		if len(a.TOTPSecret) == 0 || a.TOTPKeyVersion == rep.Current {
			continue
		}
		rep.Admins++
		if dryRun {
			continue
		}
		blob, version, err := secrets.Reseal(ctx, ring, a.TOTPSecret, a.TOTPKeyVersion, authn.TOTPAAD(a.ID))
		if err != nil {
			return rep, fmt.Errorf("administrator %s: %w", a.Username, err)
		}
		if err := store.Admins().ResealTOTP(ctx, a.ID, blob, version); err != nil {
			return rep, err
		}
	}

	channels, version, err := repo.LoadChannelSettings(ctx, store.Settings())
	if err != nil {
		return rep, err
	}
	moved := 0
	reseal := func(field string, s *repo.Sealed) error {
		if !s.IsSet() || s.KeyVersion == rep.Current {
			return nil
		}
		moved++
		if dryRun {
			return nil
		}
		blob, v, err := secrets.Reseal(ctx, ring, s.Ciphertext, s.KeyVersion, repo.ChannelSecretAAD(field))
		if err != nil {
			return fmt.Errorf("setting %s: %w", field, err)
		}
		s.Ciphertext, s.KeyVersion = blob, v
		return nil
	}
	if err := reseal("webhook_secret", &channels.Webhook.Secret); err != nil {
		return rep, err
	}
	if err := reseal("smtp_password", &channels.SMTP.Password); err != nil {
		return rep, err
	}
	rep.Settings = moved
	if moved > 0 && !dryRun {
		value, _ := json.Marshal(channels)
		if _, err := store.Settings().Set(ctx, repo.SettingAlertChannels, value, version, "rekey"); err != nil {
			return rep, err
		}
	}

	rep.Referenced, err = referencedKeyVersions(ctx, store)
	return rep, err
}

// referencedKeyVersions is every key version still named by some row.
func referencedKeyVersions(ctx context.Context, store repo.Store) ([]string, error) {
	seen := map[string]bool{}
	versions, err := store.Credentials().KeyVersions(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		seen[v] = true
	}
	admins, err := store.Admins().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range admins {
		if len(a.TOTPSecret) > 0 {
			seen[a.TOTPKeyVersion] = true
		}
	}
	channels, _, err := repo.LoadChannelSettings(ctx, store.Settings())
	if err != nil {
		return nil, err
	}
	for _, s := range []repo.Sealed{channels.Webhook.Secret, channels.SMTP.Password} {
		if s.IsSet() {
			seen[s.KeyVersion] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		if v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, nil
}

// PruneKeyFile removes from the master key file every key version that is
// neither current nor referenced, writing a .bak first. It refuses to touch
// the file when a version it would drop is still referenced -- which is
// the point of asking.
func PruneKeyFile(path string, referenced []string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Current string            `json:"current"`
		Keys    map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("master key file is not the expected JSON: %w", err)
	}
	keep := map[string]bool{f.Current: true}
	for _, v := range referenced {
		keep[v] = true
	}
	var dropped []string
	for v := range f.Keys {
		if !keep[v] {
			dropped = append(dropped, v)
		}
	}
	sort.Strings(dropped)
	for _, v := range referenced {
		if _, ok := f.Keys[v]; !ok {
			return nil, fmt.Errorf("key version %q is referenced by the database but missing from %s", v, path)
		}
	}
	if len(dropped) == 0 {
		return nil, nil
	}
	for _, v := range dropped {
		delete(f.Keys, v)
	}
	out, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path+".bak", data, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return nil, err
	}
	return dropped, nil
}
