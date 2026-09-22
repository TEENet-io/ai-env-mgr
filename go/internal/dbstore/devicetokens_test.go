package dbstore

import (
	"errors"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestDeviceTokensAuthenticateRotateRevoke(t *testing.T) {
	s, ctx := newTestStore(t)
	now := time.Now()
	device, _ := s.Devices().EnsureByHostname(ctx, "PC-1")
	if device.Channel != repo.ChannelOSS || device.EnrolledAt != nil {
		t.Fatalf("a new device is on the bucket channel: %+v", device)
	}
	if live, _ := s.DeviceTokens().HasLive(ctx, device.ID, now); live {
		t.Fatal("no token yet")
	}
	first, err := s.DeviceTokens().Issue(ctx, device.ID)
	if err != nil || len(first) < 40 {
		t.Fatalf("issue: %q %v", first, err)
	}
	if err := s.Devices().SetEnrolled(ctx, device.ID, "203.0.113.9", now); err != nil {
		t.Fatal(err)
	}
	got, err := s.DeviceTokens().Authenticate(ctx, first, now)
	if err != nil || got.ID != device.ID || got.Channel != repo.ChannelAPI || got.EnrolledFrom != "203.0.113.9" {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	if _, err := s.DeviceTokens().Authenticate(ctx, first+"x", now); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("a wrong token: %v", err)
	}
	if at, err := s.DeviceTokens().IssuedAt(ctx, device.ID); err != nil || time.Since(at) > time.Minute {
		t.Fatalf("issued at: %v %v", at, err)
	}

	// Rotation keeps the old token for the grace period, then not.
	second, err := s.DeviceTokens().Rotate(ctx, device.ID, 10*time.Minute)
	if err != nil || second == first {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.DeviceTokens().Authenticate(ctx, first, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("the old token still works inside the grace: %v", err)
	}
	if _, err := s.DeviceTokens().Authenticate(ctx, first, now.Add(20*time.Minute)); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("the old token past its grace: %v", err)
	}
	if _, err := s.DeviceTokens().Authenticate(ctx, second, now.Add(20*time.Minute)); err != nil {
		t.Fatalf("the new token: %v", err)
	}

	// Revocation ends everything; a fresh issue supersedes at once.
	if n, _ := s.DeviceTokens().Revoke(ctx, device.ID); n != 2 {
		t.Fatalf("revoked %d, want 2 (the graced one and the live one)", n)
	}
	if _, err := s.DeviceTokens().Authenticate(ctx, second, now); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("revoked token still authenticates")
	}
	third, _ := s.DeviceTokens().Issue(ctx, device.ID)
	fourth, _ := s.DeviceTokens().Issue(ctx, device.ID)
	if _, err := s.DeviceTokens().Authenticate(ctx, third, now); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("an issue must retire the previous token at once")
	}
	if _, err := s.DeviceTokens().Authenticate(ctx, fourth, now); err != nil {
		t.Fatal(err)
	}
	// A forgotten machine's token stops working even though it is live.
	s.Devices().Revoke(ctx, device.ID)
	if _, err := s.DeviceTokens().Authenticate(ctx, fourth, now); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("a forgotten machine must not authenticate")
	}
}

func TestReenrolWindowAndLogTail(t *testing.T) {
	s, ctx := newTestStore(t)
	device, _ := s.Devices().EnsureByHostname(ctx, "PC-2")
	until := time.Now().Add(time.Hour)
	if err := s.Devices().AllowReenrol(ctx, device.ID, until); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Devices().ByID(ctx, device.ID)
	if d.ReenrolAllowedUntil == nil || d.ReenrolAllowedUntil.Before(until.Add(-time.Second)) {
		t.Fatalf("window not stored: %+v", d)
	}
	// Enrolling closes the window.
	s.Devices().SetEnrolled(ctx, device.ID, "198.51.100.4", time.Now())
	if d, _ = s.Devices().ByID(ctx, device.ID); d.ReenrolAllowedUntil != nil {
		t.Fatal("enrolment must close the window")
	}
	at := time.Now().Truncate(time.Second)
	if err := s.Devices().SetLogTail(ctx, device.ID, "line1\nline2", at); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.Devices().ByID(ctx, device.ID); d.LogTail != "line1\nline2" || d.LogTailAt == nil || !d.LogTailAt.Equal(at) {
		t.Fatalf("log tail: %+v", d)
	}
}

func TestCredentialBundlesFollowTheEpoch(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	if _, _, err := s.CredentialBundles().Live(ctx, e.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("no bundle yet: %v", err)
	}
	if err := s.CredentialBundles().Put(ctx, e.ID, e.AuthEpoch, []byte("zip-1"), "etag-1"); err != nil {
		t.Fatal(err)
	}
	zip, etag, err := s.CredentialBundles().Live(ctx, e.ID)
	if err != nil || string(zip) != "zip-1" || etag != "etag-1" {
		t.Fatalf("live = %s %s %v", zip, etag, err)
	}
	// A re-issue moves the epoch on; the old bundle is no longer live.
	bumped, err := s.Employees().BumpAuthEpoch(ctx, e.ID, e.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CredentialBundles().Live(ctx, e.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("the old epoch's bundle must not be live")
	}
	s.CredentialBundles().Put(ctx, e.ID, bumped.AuthEpoch, []byte("zip-2"), "etag-2")
	if zip, _, _ := s.CredentialBundles().Live(ctx, e.ID); string(zip) != "zip-2" {
		t.Fatal("the new epoch's bundle is live")
	}
	if err := s.CredentialBundles().Purge(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CredentialBundles().Live(ctx, e.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("purged")
	}
}
