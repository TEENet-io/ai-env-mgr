package dbstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func newAdmin(t *testing.T, ctx context.Context, s *Store, username string) repo.Admin {
	t.Helper()
	a, err := s.Admins().Create(ctx, repo.NewAdmin{
		Username: username, Email: username + "@example.com",
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaA",
		Role:         repo.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("create admin %s: %v", username, err)
	}
	return a
}

func TestFirstRunHasNoWayIn(t *testing.T) {
	s, ctx := newTestStore(t)

	// Zero is what the console's one-time bootstrap page asks about. It has to
	// be a count, not "did the read fail" -- a database that cannot be read
	// must not look like a console with no accounts.
	n, err := s.Admins().CountEnabled(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a fresh database has %d administrators, want none", n)
	}

	a := newAdmin(t, ctx, s, "Zhang")
	// People type their own name with a capital about half the time, and
	// "Zhang" failing while "zhang" works is a support call every time.
	if a.Username != "zhang" {
		t.Errorf("username = %q, want it lower-cased", a.Username)
	}
	if a.Role != repo.RoleAdmin || !a.Enabled() || a.TOTPEnrolled() {
		t.Errorf("new admin = %+v, want an enabled admin with no second factor yet", a)
	}

	if n, err := s.Admins().CountEnabled(ctx); err != nil || n != 1 {
		t.Errorf("count = %d (%v), want 1", n, err)
	}
	if _, err := s.Admins().Create(ctx, repo.NewAdmin{
		Username: "ZHANG", PasswordHash: "x",
	}); !errors.Is(err, repo.ErrDuplicate) {
		t.Errorf("reusing a user name: error = %v, want ErrDuplicate", err)
	}
	if _, err := s.Admins().Create(ctx, repo.NewAdmin{Username: "nobody"}); err == nil {
		t.Error("an account with no password hash was created")
	}
}

func TestEnrolmentStoresTheSealedSeedAndRecoveryCodes(t *testing.T) {
	s, ctx := newTestStore(t)
	a := newAdmin(t, ctx, s, "zhang")

	sealed := []byte("sealed-seed-blob")
	if err := s.Admins().SetTOTP(ctx, a.ID, sealed, "k1", []string{"hash-1", "hash-2"}); err != nil {
		t.Fatalf("set totp: %v", err)
	}
	enrolled, err := s.Admins().ByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if !enrolled.TOTPEnrolled() || !bytes.Equal(enrolled.TOTPSecret, sealed) || enrolled.TOTPKeyVersion != "k1" {
		t.Fatalf("enrolled admin = %+v, want the sealed seed and its key version", enrolled)
	}
	if len(enrolled.RecoveryHashes) != 2 {
		t.Errorf("recovery hashes = %v, want both", enrolled.RecoveryHashes)
	}

	// Enrolling a second factor with no way back is how somebody locks
	// themselves out of the console with a lost phone.
	if err := s.Admins().SetTOTP(ctx, a.ID, sealed, "k1", nil); err == nil {
		t.Error("enrolment without recovery codes was accepted")
	}
	if err := s.Admins().SetTOTP(ctx, a.ID, sealed, "", []string{"h"}); err == nil {
		t.Error("a sealed seed with no key version was accepted; it could never be opened")
	}

	// Using a code consumes it: one that still works afterwards is a password
	// with extra steps.
	if err := s.Admins().ReplaceRecoveryHashes(ctx, a.ID, []string{"hash-2"}); err != nil {
		t.Fatalf("replace recovery hashes: %v", err)
	}
	after, err := s.Admins().ByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if len(after.RecoveryHashes) != 1 || after.RecoveryHashes[0] != "hash-2" {
		t.Errorf("recovery hashes = %v, want only the unused one", after.RecoveryHashes)
	}
}

func TestSessionsAreKeyedByAHashAndExpireTwoWays(t *testing.T) {
	s, ctx := newTestStore(t)
	a := newAdmin(t, ctx, s, "zhang")

	token := "a-cookie-value-nobody-else-has"
	sum := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	if err := s.Admins().CreateSession(ctx, repo.Session{
		TokenSHA256: sum[:], PrincipalID: a.ID,
		ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour),
		CreatedIP: "203.0.113.7", UserAgent: "Firefox",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	session, owner, err := s.Admins().SessionByToken(ctx, sum[:])
	if err != nil {
		t.Fatalf("SessionByToken: %v", err)
	}
	if owner.ID != a.ID || session.CreatedIP != "203.0.113.7" || session.UserAgent != "Firefox" {
		t.Errorf("session = %+v owned by %s, want zhang's with its address", session, owner.ID)
	}

	// The cookie itself is never a key: a dump or a slow query log must not
	// hand anybody a working session.
	if _, _, err := s.Admins().SessionByToken(ctx, []byte(token)); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("looking a session up by the raw cookie: error = %v, want ErrNotFound", err)
	}
	if err := s.Admins().CreateSession(ctx, repo.Session{
		TokenSHA256: []byte("short"), PrincipalID: a.ID,
		ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour),
	}); err == nil {
		t.Error("a session keyed by something that is not a SHA-256 was created")
	}

	// The absolute expiry is never pushed out: refreshing on activity alone
	// would let one sign-in last for ever behind an open tab.
	if err := s.Admins().TouchSession(ctx, sum[:], now.Add(72*time.Hour)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	refreshed, _, err := s.Admins().SessionByToken(ctx, sum[:])
	if err != nil {
		t.Fatalf("SessionByToken: %v", err)
	}
	if refreshed.ExpiresAt.After(refreshed.AbsoluteExpiresAt) {
		t.Errorf("idle expiry %v is past the absolute one %v", refreshed.ExpiresAt, refreshed.AbsoluteExpiresAt)
	}

	// A session that outlives the account it belongs to is the thing being
	// prevented; asking every caller to check would eventually find one that
	// does not.
	other := newAdmin(t, ctx, s, "li")
	if _, err := s.Admins().SetDisabled(ctx, a.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, _, err := s.Admins().SessionByToken(ctx, sum[:]); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("a disabled account's session still resolves: %v", err)
	}
	if _, err := s.Admins().SetDisabled(ctx, a.ID, false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	n, err := s.Admins().DeleteSessionsFor(ctx, a.ID)
	if err != nil {
		t.Fatalf("delete sessions: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d sessions, want 1", n)
	}
	_ = other
}

func TestExpiredSessionsAreGoneAndCleanable(t *testing.T) {
	s, ctx := newTestStore(t)
	a := newAdmin(t, ctx, s, "zhang")
	past := time.Now().UTC().Add(-time.Hour)
	sum := sha256.Sum256([]byte("old"))
	if err := s.Admins().CreateSession(ctx, repo.Session{
		TokenSHA256: sum[:], PrincipalID: a.ID,
		ExpiresAt: past, AbsoluteExpiresAt: past.Add(time.Minute),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, _, err := s.Admins().SessionByToken(ctx, sum[:]); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("an expired session resolved: %v", err)
	}
	if err := s.Admins().TouchSession(ctx, sum[:], time.Now().Add(time.Hour)); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("an expired session was refreshed back to life: %v", err)
	}
	n, err := s.Admins().DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if n != 1 {
		t.Errorf("cleared %d expired sessions, want 1", n)
	}
}

func TestTheLastWayInCannotBeDisabled(t *testing.T) {
	s, ctx := newTestStore(t)
	only := newAdmin(t, ctx, s, "zhang")

	// Disabling the last account locks everybody out, including whoever would
	// have to undo it.
	_, err := s.Admins().SetDisabled(ctx, only.ID, true)
	if err == nil {
		t.Fatal("the only administrator was disabled")
	}
	if !strings.Contains(err.Error(), "last account") {
		t.Errorf("error = %v, want it to say why", err)
	}

	second := newAdmin(t, ctx, s, "li")
	if _, err := s.Admins().SetDisabled(ctx, only.ID, true); err != nil {
		t.Fatalf("disabling one of two: %v", err)
	}
	if _, err := s.Admins().SetDisabled(ctx, second.ID, true); err == nil {
		t.Error("the last remaining enabled account was disabled")
	}
}

func TestFailedAttemptsAreCountedTwoWays(t *testing.T) {
	s, ctx := newTestStore(t)
	newAdmin(t, ctx, s, "zhang")

	for range 3 {
		if err := s.Admins().RecordAttempt(ctx, repo.LoginAttempt{
			Username: "zhang", SourceIP: "203.0.113.7", Outcome: repo.LoginBadPassword,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	// Somebody trying one password against many accounts shows up in the
	// address count, not in any one account's.
	for _, user := range []string{"li", "wang", "chen"} {
		if err := s.Admins().RecordAttempt(ctx, repo.LoginAttempt{
			Username: user, SourceIP: "203.0.113.7", Outcome: repo.LoginUnknownUser,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := s.Admins().RecordAttempt(ctx, repo.LoginAttempt{
		Username: "zhang", SourceIP: "203.0.113.7", Outcome: repo.LoginOK,
	}); err != nil {
		t.Fatalf("record success: %v", err)
	}

	byUser, byIP, err := s.Admins().RecentFailures(ctx, "ZHANG", "203.0.113.7", time.Hour)
	if err != nil {
		t.Fatalf("recent failures: %v", err)
	}
	if byUser != 3 {
		t.Errorf("failures for the account = %d, want 3 (the success must not count)", byUser)
	}
	if byIP != 6 {
		t.Errorf("failures from the address = %d, want 6", byIP)
	}

	// An attempt with no address recorded must not blow up the count query.
	if err := s.Admins().RecordAttempt(ctx, repo.LoginAttempt{
		Username: "zhang", Outcome: repo.LoginBadPassword,
	}); err != nil {
		t.Fatalf("record without an address: %v", err)
	}
	if _, _, err := s.Admins().RecentFailures(ctx, "zhang", "", time.Hour); err != nil {
		t.Fatalf("counting with no address: %v", err)
	}

	// Old attempts fall out of the window rather than locking somebody out for
	// ever.
	if _, err := s.q.Exec(ctx,
		`update admin_login_attempts set at = now() - interval '2 hours'`); err != nil {
		t.Fatalf("age the attempts: %v", err)
	}
	byUser, byIP, err = s.Admins().RecentFailures(ctx, "zhang", "203.0.113.7", 15*time.Minute)
	if err != nil {
		t.Fatalf("recent failures: %v", err)
	}
	if byUser != 0 || byIP != 0 {
		t.Errorf("failures = %d/%d after the window passed, want none", byUser, byIP)
	}
}
