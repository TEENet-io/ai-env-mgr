package admincore

import (
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// A read that fails for a reason other than "not there" must never be treated
// as an empty policy: the mutators build the next version on top of what they
// read, so a transient OSS error would publish a policy with no blocked
// domains, no AppLocker allow list and no target versions -- unblocking every
// machine on the next sync.
func TestPolicyMutatorsRefuseToWriteAfterAFailedRead(t *testing.T) {
	seeded := `{"blockEnabled":true,"blockedDomains":["openai.com"],` +
		`"appLockerAllowPaths":["C:\\tools\\Codex\\*"],"appLockerMode":"Enforce",` +
		`"syncIntervalMinutes":5,"agentUpdateVersion":"1.2.15"}`

	for _, tc := range []struct {
		name   string
		break_ func(*fakeStore)
	}{
		{"transient read error", func(fs *fakeStore) {
			fs.getErrFor = ossclient.PolicyKey()
			fs.getErr = errors.New("dial tcp: i/o timeout")
		}},
		{"corrupt object", func(fs *fakeStore) {
			fs.objects[ossclient.PolicyKey()] = []byte("{not json")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, op := range []struct {
				name string
				call func(*Manager) error
			}{
				{"MutateDomains", func(m *Manager) error { _, err := m.MutateDomains([]string{"x.com"}, nil); return err }},
				{"SetBlockEnabled", func(m *Manager) error { _, err := m.SetBlockEnabled(false); return err }},
				{"SetSyncInterval", func(m *Manager) error { _, err := m.SetSyncInterval(30); return err }},
				{"MutateAppLockerAllowPaths", func(m *Manager) error {
					_, err := m.MutateAppLockerAllowPaths([]string{`C:\tools\Other\*`}, nil)
					return err
				}},
				{"SetAppLockerMode", func(m *Manager) error { _, err := m.SetAppLockerMode("audit"); return err }},
				{"SetCollect", func(m *Manager) error { _, err := m.SetCollect(true, nil, nil); return err }},
			} {
				m, fs := newManager()
				fs.objects[ossclient.PolicyKey()] = []byte(seeded)
				tc.break_(fs)
				before := string(fs.objects[ossclient.PolicyKey()])

				if err := op.call(m); err == nil {
					t.Errorf("%s: wrote after a failed read", op.name)
				}
				if got := string(fs.objects[ossclient.PolicyKey()]); got != before {
					t.Errorf("%s: policy was overwritten:\n before %s\n after  %s", op.name, before, got)
				}
			}
		})
	}
}

// Missing is different from broken: a bucket with no policy yet is the state a
// fresh deployment starts in, and the first write has to be allowed.
func TestPolicyMutatorsStillWorkWhenThereIsNoPolicyYet(t *testing.T) {
	m, fs := newManager()
	p, err := m.SetBlockEnabled(true)
	if err != nil {
		t.Fatalf("first write on an empty bucket: %v", err)
	}
	if !p.BlockEnabled || len(fs.objects[ossclient.PolicyKey()]) == 0 {
		t.Fatalf("expected the first policy to be published, got %+v", p)
	}
}

// Same rule for the quota defaults: onboarding reads them when the form leaves
// the quota blank, and a transient failure must not silently onboard somebody
// on the built-in defaults instead of the configured ones.
func TestQuotaDefaultsReportFailuresInsteadOfReturningBuiltins(t *testing.T) {
	m, fs := newManager()
	if q, err := m.LoadQuotaDefaults(); err != nil || q != DefaultQuota {
		t.Fatalf("no object yet should read as the built-in default: %v %v", q, err)
	}

	fs.getErrFor = QuotaDefaultsKey()
	fs.getErr = errors.New("dial tcp: i/o timeout")
	if _, err := m.LoadQuotaDefaults(); err == nil {
		t.Error("a transient read failure was reported as the built-in default")
	}

	fs.getErrFor, fs.getErr = "", nil
	fs.objects[QuotaDefaultsKey()] = []byte("{not json")
	if _, err := m.LoadQuotaDefaults(); err == nil {
		t.Error("a corrupt object was reported as the built-in default")
	}

	fs.objects[QuotaDefaultsKey()] = []byte(`{"monthlyBudgetUSD":-5,"rpm":0,"tpm":0,"parallel":0}`)
	if _, err := m.LoadQuotaDefaults(); err == nil {
		t.Error("an invalid stored quota was reported as the built-in default")
	} else if !strings.Contains(err.Error(), "quota") {
		t.Errorf("error should name what was wrong, got %v", err)
	}
}
