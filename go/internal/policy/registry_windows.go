//go:build windows

package policy

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"golang.org/x/sys/windows/registry"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// target pairs one browser's registry key with the values it should hold.
type target struct {
	browser string
	keyPath string
	values  []string
}

// Apply writes or clears the browser blocklist policies under
// HKEY_LOCAL_MACHINE according to p.
//
// Everything here is written under HKLM rather than HKCU. HKLM requires
// administrative rights to modify, so a standard-user employee account can
// see the policy take effect in their browser but cannot edit or delete
// these values to unblock a site themselves -- only provisioning/admin
// tooling running elevated can.
//
// BlockEnabled == false deletes all three policy keys outright, rather than
// writing an empty list. This is the escape hatch for an administrator who
// needs to temporarily lift the block: an absent policy key is the one
// state that Chrome, Edge and Firefox all unambiguously treat as
// "unmanaged", whereas an empty URLBlocklist/WebsiteFilter list is not
// guaranteed to behave the same way across all three.
//
// BlockEnabled == true rebuilds each browser's key from scratch: every
// existing value is deleted, the key is recreated, and the domains are
// written back as sequentially numbered values ("1", "2", "3", ...), which
// is the list format all three policy engines require. The policy in p is
// always the complete desired state, never a delta, so a full rebuild is
// the only way to guarantee the registry ends up matching it exactly. It
// also has the side effect of automatically clearing out any numbered
// value left behind by a domain that used to be blocked but has since been
// removed from BlockedDomains -- there is no old state to reconcile
// against, so nothing can be "forgotten" and left blocking a domain that
// should now be allowed.
func Apply(p model.Policy) error {
	targets := []target{
		{"Chrome", ChromeKey, nil},
		{"Edge", EdgeKey, nil},
		{"Firefox", FirefoxKey, nil},
	}

	if !p.BlockEnabled {
		for _, t := range targets {
			if err := deleteKeyTree(t.keyPath); err != nil {
				return fmt.Errorf("clear %s policy: %w", t.browser, err)
			}
		}
		return nil
	}

	targets[0].values = ChromiumEntries(p.BlockedDomains)
	targets[1].values = ChromiumEntries(p.BlockedDomains)
	targets[2].values = FirefoxEntries(p.BlockedDomains)

	for _, t := range targets {
		if err := rebuildKey(t.keyPath, t.values); err != nil {
			return fmt.Errorf("apply %s policy: %w", t.browser, err)
		}
	}
	return nil
}

// rebuildKey deletes keyPath under HKLM if present, recreates it, and
// writes values under sequential names "1", "2", "3", ... -- the ordered
// list format Chrome/Edge/Firefox's group policy engines all expect.
func rebuildKey(keyPath string, values []string) error {
	if err := deleteKeyTree(keyPath); err != nil {
		return err
	}

	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, keyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create key %s: %w", keyPath, err)
	}
	defer k.Close()

	for i, v := range values {
		name := strconv.Itoa(i + 1)
		if err := k.SetStringValue(name, v); err != nil {
			return fmt.Errorf(`set value %s\%s: %w`, keyPath, name, err)
		}
	}
	return nil
}

// deleteKeyTree removes keyPath under HKLM. It is a no-op if the key is
// already absent.
//
// A registry key that still holds values cannot always be removed with a
// bare DeleteKey call, so every value under the key is deleted first and
// the now-empty key is deleted second.
func deleteKeyTree(keyPath string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, keyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open key %s: %w", keyPath, err)
	}

	names, err := k.ReadValueNames(-1)
	if err != nil {
		k.Close()
		return fmt.Errorf("list values under %s: %w", keyPath, err)
	}
	for _, name := range names {
		if err := k.DeleteValue(name); err != nil {
			k.Close()
			return fmt.Errorf(`delete value %s\%s: %w`, keyPath, name, err)
		}
	}
	k.Close()

	if err := registry.DeleteKey(registry.LOCAL_MACHINE, keyPath); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("delete key %s: %w", keyPath, err)
	}
	return nil
}

// Current reads back the browser block policy this machine currently has
// applied.
//
// It exists so `agent.exe status` can report local state without running a
// sync: an operator inspecting a machine must not, as a side effect, rewrite
// the registry and restart the employee's AI tools.
//
// The registry is the source of truth here, not any cached marker file: what
// matters is what the browsers will actually enforce. An absent Chrome key
// means unmanaged, which is reported as BlockEnabled == false.
//
// This does not read the AppLocker allow list: unlike the Chrome block list,
// a policy value here cannot be "the desired state" -- an operator asking
// "what is really applied" needs a positive success/failure signal (0 rules
// confirmed vs. could not read), which a plain int field on this policy
// object cannot carry. See policy.LocalAppLockerPaths, which callers that
// need that answer call directly instead.
func Current() (model.Policy, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, ChromeKey, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return model.Policy{BlockEnabled: false}, nil
		}
		return model.Policy{}, fmt.Errorf("read Chrome policy: %w", err)
	}
	defer k.Close()

	names, err := k.ReadValueNames(0)
	if err != nil {
		return model.Policy{}, fmt.Errorf("list Chrome policy values: %w", err)
	}

	// Values are named "1", "2", ... so read them back in numeric order to
	// report the list the way it was written.
	sort.Slice(names, func(i, j int) bool {
		a, errA := strconv.Atoi(names[i])
		b, errB := strconv.Atoi(names[j])
		if errA != nil || errB != nil {
			return names[i] < names[j]
		}
		return a < b
	})

	domains := make([]string, 0, len(names))
	for _, n := range names {
		v, _, err := k.GetStringValue(n)
		if err != nil {
			continue
		}
		// Chromium entries are the bare domain, written one per value by
		// ChromiumEntries, so the value is the domain.
		if v != "" {
			domains = append(domains, v)
		}
	}
	return model.Policy{BlockEnabled: len(domains) > 0, BlockedDomains: domains}, nil
}
