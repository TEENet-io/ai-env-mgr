package dbstore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func newGrant(t *testing.T, ctx context.Context, s *Store, e repo.Employee, alias string) repo.Grant {
	t.Helper()
	g, err := s.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: e.ID, Epoch: e.AuthEpoch,
		ExternalUser: "emp-" + e.WindowsUser, KeyAlias: alias,
		Models: []string{"claude-4.5-sonnet"},
	})
	if err != nil {
		t.Fatalf("create grant %s: %v", alias, err)
	}
	return g
}

func TestOneLiveGrantPerEmployee(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")

	first := newGrant(t, ctx, s, e, "work1-1")
	if first.Desired != repo.GrantActive || first.Actual != repo.ActualUnknown {
		t.Fatalf("new grant = %+v, want active intent and an unknown observed state", first)
	}
	// Unknown until something has actually asked the gateway. A row that
	// claimed 'active' on creation would be the console believing its own
	// intent.
	if first.ReconciledAt != nil {
		t.Error("a grant nobody has checked has a reconciliation time")
	}

	// Two live tokens for one person means revoking "the" token leaves the
	// other one working -- a leak nobody sees.
	if _, err := s.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: e.ID, Epoch: e.AuthEpoch,
		ExternalUser: "emp-work1", KeyAlias: "work1-2",
	}); !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("a second active grant: error = %v, want ErrDuplicate", err)
	}

	revoked, err := s.Grants().Revoke(ctx, first.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked.Desired != repo.GrantRevoked {
		t.Fatalf("revoked grant = %+v", revoked)
	}
	// Revoking runs from a task, and a task runs at least once.
	if _, err := s.Grants().Revoke(ctx, first.ID); err != nil {
		t.Errorf("revoking twice: %v", err)
	}

	second := newGrant(t, ctx, s, e, "work1-2")
	live, err := s.Grants().Active(ctx, "", e.ID)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if live.ID != second.ID {
		t.Errorf("Active returned %s, want the new grant %s", live.ID, second.ID)
	}
	// The key alias is unique on the gateway, and the same alias twice would
	// leave us unable to say which token a revoke is about.
	if _, err := s.Grants().Create(ctx, repo.NewGrant{
		EmployeeID: e.ID, Epoch: e.AuthEpoch, ExternalUser: "emp-work1", KeyAlias: "work1-2",
	}); !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("reusing a key alias: error = %v, want ErrDuplicate", err)
	}

	all, err := s.Grants().ByEmployee(ctx, e.ID)
	if err != nil {
		t.Fatalf("ByEmployee: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ByEmployee returned %d grants, want both", len(all))
	}
	found, err := s.Grants().ByKeyAlias(ctx, "litellm", "work1-1")
	if err != nil {
		t.Fatalf("ByKeyAlias: %v", err)
	}
	if found.ID != first.ID {
		t.Errorf("ByKeyAlias found %s, want %s", found.ID, first.ID)
	}
}

func TestIntentAndObservedStateAreSeparate(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	g := newGrant(t, ctx, s, e, "work1-1")

	confirmed, err := s.Grants().RecordActual(ctx, g.ID, repo.ActualActive, "")
	if err != nil {
		t.Fatalf("record actual: %v", err)
	}
	if confirmed.Actual != repo.ActualActive || confirmed.ReconciledAt == nil {
		t.Fatalf("grant = %+v, want an observed active state with a time", confirmed)
	}
	// Nothing about what we asked for changes because of what we saw.
	if confirmed.Desired != repo.GrantActive {
		t.Error("recording the observed state changed the intent")
	}

	if _, err := s.Grants().RecordActual(ctx, g.ID, "probably", ""); err == nil {
		t.Error("a state the gateway cannot be in was recorded")
	}

	// A grant we believe is revoked but which the gateway still serves is a
	// token nobody thinks exists; it has to come back on the reconcile list.
	if _, err := s.Grants().Revoke(ctx, g.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	stale, err := s.Grants().NeedsReconcile(ctx, "litellm", time.Hour, 0)
	if err != nil {
		t.Fatalf("NeedsReconcile: %v", err)
	}
	if len(stale) != 1 || stale[0].ID != g.ID {
		t.Fatalf("NeedsReconcile = %+v, want the revoked grant the gateway still serves", stale)
	}

	if _, err := s.Grants().RecordActual(ctx, g.ID, repo.ActualMissing, ""); err != nil {
		t.Fatalf("record actual: %v", err)
	}
	// 'missing' is success for a revoke: the key is not there.
	settled, err := s.Grants().NeedsReconcile(ctx, "litellm", time.Hour, 0)
	if err != nil {
		t.Fatalf("NeedsReconcile: %v", err)
	}
	if len(settled) != 0 {
		t.Errorf("NeedsReconcile = %+v, want nothing once intent and reality agree", settled)
	}
}

func TestCredentialsKeepOneLiveVersion(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")

	if _, err := s.Credentials().Live(ctx, e.ID, repo.PurposeCodexGateway); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("Live before anything is issued: error = %v, want ErrNotFound", err)
	}

	first, err := s.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: e.ID, Epoch: e.AuthEpoch, Purpose: repo.PurposeCodexGateway,
		Ciphertext: []byte("sealed-blob-1"), KeyVersion: "k1",
		ContentSHA256: bytes.Repeat([]byte{1}, 32),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if first.RetiredAt != nil {
		t.Error("a freshly stored credential is already retired")
	}

	// Re-issuing replaces: two live credentials for one purpose would leave
	// the exporter free to deliver either, and "which token does that machine
	// have" would stop having an answer.
	second, err := s.Credentials().Store(ctx, repo.NewCredential{
		EmployeeID: e.ID, Epoch: e.AuthEpoch + 1, Purpose: repo.PurposeCodexGateway,
		Ciphertext: []byte("sealed-blob-2"), KeyVersion: "k1",
		ContentSHA256: bytes.Repeat([]byte{2}, 32),
	})
	if err != nil {
		t.Fatalf("store again: %v", err)
	}
	live, err := s.Credentials().Live(ctx, e.ID, repo.PurposeCodexGateway)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if live.ID != second.ID || string(live.Ciphertext) != "sealed-blob-2" {
		t.Errorf("live credential = %s, want the newly issued %s", live.ID, second.ID)
	}
	if live.KeyVersion != "k1" || len(live.ContentSHA256) != 32 {
		t.Errorf("credential lost its key version or content hash: %+v", live)
	}

	// The replaced one is kept, retired: it is what the machine may still have
	// on disk until it next syncs.
	old, err := s.Credentials().ByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if old.RetiredAt == nil {
		t.Error("the replaced credential was not retired")
	}

	// Offboarding ends the credential without issuing a replacement.
	n, err := s.Credentials().Retire(ctx, e.ID, repo.PurposeCodexGateway)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if n != 1 {
		t.Errorf("retired %d credentials, want 1", n)
	}
	if _, err := s.Credentials().Live(ctx, e.ID, repo.PurposeCodexGateway); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("Live after retiring: error = %v, want ErrNotFound", err)
	}
	// Asking for a state, not for an event: having nothing to retire is fine.
	if n, err := s.Credentials().Retire(ctx, e.ID, repo.PurposeCodexGateway); err != nil || n != 0 {
		t.Errorf("retiring nothing: %d, %v", n, err)
	}
}

func TestCredentialsRefuseWhatCannotBeOpenedAgain(t *testing.T) {
	s, ctx := newTestStore(t)
	e := mustCreate(t, ctx, s, "work1")
	ok := repo.NewCredential{
		EmployeeID: e.ID, Epoch: 1, Purpose: repo.PurposeCodexGateway,
		Ciphertext: []byte("sealed"), KeyVersion: "k1",
	}

	// Without the key version the row can never be opened. Cheaper to refuse
	// than to find out during an incident.
	noVersion := ok
	noVersion.KeyVersion = ""
	if _, err := s.Credentials().Store(ctx, noVersion); err == nil {
		t.Error("a credential with no key version was stored")
	}
	empty := ok
	empty.Ciphertext = nil
	if _, err := s.Credentials().Store(ctx, empty); err == nil {
		t.Error("a credential with no ciphertext was stored")
	}
	noEpoch := ok
	noEpoch.Epoch = 0
	if _, err := s.Credentials().Store(ctx, noEpoch); err == nil {
		t.Error("a credential with no epoch was stored")
	}
	missing := ok
	missing.EmployeeID = "6f1e9e6c-0000-4000-8000-000000000000"
	if _, err := s.Credentials().Store(ctx, missing); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("a credential for an unknown employee: error = %v, want ErrNotFound", err)
	}
}
