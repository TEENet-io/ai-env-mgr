package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestUnboundSelfEnrolledDevicesArePrunedAfterAWeek(t *testing.T) {
	store, ctx := newWorkerStore(t)
	now := time.Now()
	employee, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1"})
	enrolled := func(name string, ago time.Duration) repo.Device {
		d, _ := store.Devices().EnsureByHostname(ctx, name)
		store.DeviceTokens().Issue(ctx, d.ID)
		store.Devices().SetEnrolled(ctx, d.ID, "203.0.113.1", now.Add(-ago))
		return d
	}
	stale := enrolled("STRAY-OLD", 8*24*time.Hour)
	fresh := enrolled("STRAY-NEW", 2*24*time.Hour)
	assigned := enrolled("PC-1", 30*24*time.Hour)
	store.Bindings().Bind(ctx, assigned.ID, employee.ID, "", "zhang")
	onceBound := enrolled("PC-2", 30*24*time.Hour)
	store.Bindings().Bind(ctx, onceBound.ID, employee.ID, "", "zhang")
	store.Bindings().Unbind(ctx, onceBound.ID, "zhang")
	bucket, _ := store.Devices().EnsureByHostname(ctx, "OLD-AGENT") // never enrolled; oss channel

	res, err := DevicePrune{Store: store, Now: func() time.Time { return now }}.Run(ctx, repo.Task{})
	if err != nil || !strings.HasPrefix(res.Note, "forgot 1 machine") {
		t.Fatalf("prune: %q %v", res.Note, err)
	}
	for _, c := range []struct {
		d    repo.Device
		gone bool
	}{{stale, true}, {fresh, false}, {assigned, false}, {onceBound, false}, {bucket, false}} {
		d, _ := store.Devices().ByID(ctx, c.d.ID)
		if (d.Status == repo.DeviceRevoked) != c.gone {
			t.Errorf("%s: revoked=%v, want %v", d.Hostname, d.Status == repo.DeviceRevoked, c.gone)
		}
	}
	if live, _ := store.DeviceTokens().HasLive(ctx, stale.ID, now); live {
		t.Fatal("the pruned machine's token must be revoked")
	}
	events, _, _ := store.Audit().Search(ctx, repo.AuditFilter{Action: "device.prune", Limit: 5})
	if len(events) != 1 || events[0].TargetID != stale.ID {
		t.Fatalf("audit = %+v", events)
	}
}

func TestSwitchingTheBucketChannelOffStopsTheObjectsButNotTheBundles(t *testing.T) {
	store, service, objects, w, ctx := exporting(t)
	if _, err := service.PublishPolicy(ctx, []byte(`{"blockEnabled":true,"syncIntervalMinutes":30}`), "t", "zhang", ""); err != nil {
		t.Fatal(err)
	}
	employee := onboard(t, ctx, service, "work1")
	drain(t, ctx, w)
	if _, ok := objects.get(ossclient.PolicyKey()); !ok {
		t.Fatal("with the channel on, the policy object is written")
	}
	if _, _, err := store.CredentialBundles().Live(ctx, employee.ID); err != nil {
		t.Fatalf("the bundle is stored alongside: %v", err)
	}

	store.Settings().Set(ctx, repo.SettingDeviceChannel, []byte(`{"write_oss_objects":false,"import_oss_status":false}`), 0, "zhang")
	objects.mu.Lock()
	objects.objects = map[string][]byte{}
	objects.mu.Unlock()
	if _, err := service.PublishPolicy(ctx, []byte(`{"blockEnabled":false,"syncIntervalMinutes":30}`), "t", "zhang", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reissue(ctx, employee.ID, "zhang", ""); err != nil {
		t.Fatal(err)
	}
	drain(t, ctx, w)
	if len(objects.objects) != 0 {
		keys := []string{}
		for k := range objects.objects {
			keys = append(keys, k)
		}
		t.Fatalf("with the channel off nothing is written to the bucket, got %v", keys)
	}
	if _, etag, err := store.CredentialBundles().Live(ctx, employee.ID); err != nil || etag == "" {
		t.Fatalf("the new epoch's bundle is still stored: %v", err)
	}
	res, err := StatusImport{Store: store, Objects: objects}.Run(ctx, repo.Task{})
	if err != nil || res.Note != "oss status import off" {
		t.Fatalf("status import: %q %v", res.Note, err)
	}
}
