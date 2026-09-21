package authn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

// fakeAdmins is an in-memory repo.Admins. The sign-in logic is what is under
// test here; the SQL has its own tests against a real database.
type fakeAdmins struct {
	byID     map[string]repo.Admin
	sessions map[string]repo.Session
	attempts []repo.LoginAttempt
	failErr  error
}

func newFakeAdmins() *fakeAdmins {
	return &fakeAdmins{byID: map[string]repo.Admin{}, sessions: map[string]repo.Session{}}
}

func (f *fakeAdmins) ByID(_ context.Context, id string) (repo.Admin, error) {
	a, ok := f.byID[id]
	if !ok {
		return repo.Admin{}, repo.ErrNotFound
	}
	return a, nil
}

func (f *fakeAdmins) ByUsername(_ context.Context, username string) (repo.Admin, error) {
	if f.failErr != nil {
		return repo.Admin{}, f.failErr
	}
	for _, a := range f.byID {
		if a.Username == strings.ToLower(strings.TrimSpace(username)) {
			return a, nil
		}
	}
	return repo.Admin{}, repo.ErrNotFound
}

func (f *fakeAdmins) List(context.Context) ([]repo.Admin, error) {
	out := []repo.Admin{}
	for _, a := range f.byID {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeAdmins) CountEnabled(context.Context) (int, error) {
	n := 0
	for _, a := range f.byID {
		if a.Enabled() {
			n++
		}
	}
	return n, nil
}

func (f *fakeAdmins) Create(_ context.Context, n repo.NewAdmin) (repo.Admin, error) {
	username := strings.ToLower(strings.TrimSpace(n.Username))
	for _, a := range f.byID {
		if a.Username == username {
			return repo.Admin{}, repo.ErrDuplicate
		}
	}
	a := repo.Admin{
		ID: fmt.Sprintf("admin-%d", len(f.byID)+1), Username: username, Email: n.Email,
		PasswordHash: n.PasswordHash, Role: n.Role, CreatedAt: time.Now().UTC(),
	}
	if a.Role == "" {
		a.Role = repo.RoleAdmin
	}
	f.byID[a.ID] = a
	return a, nil
}

func (f *fakeAdmins) SetPasswordHash(_ context.Context, id, hash string) error {
	a, ok := f.byID[id]
	if !ok {
		return repo.ErrNotFound
	}
	a.PasswordHash = hash
	f.byID[id] = a
	return nil
}

func (f *fakeAdmins) ResealTOTP(_ context.Context, id string, sealed []byte, keyVersion string) error {
	a, ok := f.byID[id]
	if !ok {
		return repo.ErrNotFound
	}
	a.TOTPSecret, a.TOTPKeyVersion = sealed, keyVersion
	f.byID[id] = a
	return nil
}

func (f *fakeAdmins) SetTOTP(_ context.Context, id string, sealed []byte, keyVersion string, hashes []string) error {
	a, ok := f.byID[id]
	if !ok {
		return repo.ErrNotFound
	}
	now := time.Now().UTC()
	a.TOTPSecret, a.TOTPKeyVersion, a.TOTPEnrolledAt, a.RecoveryHashes = sealed, keyVersion, &now, hashes
	f.byID[id] = a
	return nil
}

func (f *fakeAdmins) ReplaceRecoveryHashes(_ context.Context, id string, hashes []string) error {
	a, ok := f.byID[id]
	if !ok {
		return repo.ErrNotFound
	}
	a.RecoveryHashes = hashes
	f.byID[id] = a
	return nil
}

func (f *fakeAdmins) RecordLogin(_ context.Context, id string, at time.Time) error {
	a, ok := f.byID[id]
	if !ok {
		return repo.ErrNotFound
	}
	a.LastLoginAt = &at
	f.byID[id] = a
	return nil
}

func (f *fakeAdmins) SetDisabled(_ context.Context, id string, disabled bool) (repo.Admin, error) {
	a, ok := f.byID[id]
	if !ok {
		return repo.Admin{}, repo.ErrNotFound
	}
	if disabled {
		now := time.Now().UTC()
		a.DisabledAt = &now
	} else {
		a.DisabledAt = nil
	}
	f.byID[id] = a
	return a, nil
}

func (f *fakeAdmins) RecordAttempt(_ context.Context, a repo.LoginAttempt) error {
	f.attempts = append(f.attempts, a)
	return nil
}

func (f *fakeAdmins) RecentFailures(_ context.Context, username, sourceIP string, window time.Duration) (int, int, error) {
	cutoff := time.Now().UTC().Add(-window)
	byUser, byIP := 0, 0
	for _, a := range f.attempts {
		if a.Outcome == repo.LoginOK || a.At.Before(cutoff) {
			continue
		}
		if a.Username == strings.ToLower(username) {
			byUser++
		}
		if sourceIP != "" && a.SourceIP == sourceIP {
			byIP++
		}
	}
	return byUser, byIP, nil
}

func (f *fakeAdmins) CreateSession(_ context.Context, s repo.Session) error {
	f.sessions[string(s.TokenSHA256)] = s
	return nil
}

func (f *fakeAdmins) SessionByToken(_ context.Context, token []byte) (repo.Session, repo.Admin, error) {
	s, ok := f.sessions[string(token)]
	if !ok || s.ExpiresAt.Before(time.Now()) || s.AbsoluteExpiresAt.Before(time.Now()) {
		return repo.Session{}, repo.Admin{}, repo.ErrNotFound
	}
	a := f.byID[s.PrincipalID]
	if !a.Enabled() {
		return repo.Session{}, repo.Admin{}, repo.ErrNotFound
	}
	return s, a, nil
}

func (f *fakeAdmins) TouchSession(_ context.Context, token []byte, expiresAt time.Time) error {
	s, ok := f.sessions[string(token)]
	if !ok {
		return repo.ErrNotFound
	}
	if expiresAt.After(s.AbsoluteExpiresAt) {
		expiresAt = s.AbsoluteExpiresAt
	}
	s.ExpiresAt = expiresAt
	f.sessions[string(token)] = s
	return nil
}

func (f *fakeAdmins) DeleteSession(_ context.Context, token []byte) error {
	delete(f.sessions, string(token))
	return nil
}

func (f *fakeAdmins) DeleteSessionsFor(_ context.Context, principalID string) (int, error) {
	n := 0
	for key, s := range f.sessions {
		if s.PrincipalID == principalID {
			delete(f.sessions, key)
			n++
		}
	}
	return n, nil
}

func (f *fakeAdmins) DeleteExpiredSessions(context.Context) (int, error) { return 0, nil }

func testKeyring(t *testing.T) secrets.Keyring {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	body := `{"current":"k1","keys":{"k1":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ring, err := secrets.NewFileKeyring(path)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}

// enrolled sets up an account that has finished enrolment, and returns the
// service, the account and its TOTP seed.
func enrolled(t *testing.T) (*Service, *fakeAdmins, repo.Admin, string, string) {
	t.Helper()
	admins := newFakeAdmins()
	svc := New(admins, testKeyring(t), "windows-control.teenet.app")
	ctx := context.Background()

	admin, password, err := svc.CreateAccount(ctx, "Zhang", "zhang@example.com", repo.RoleAdmin)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	secret, uri, err := svc.BeginEnrolment(admin)
	if err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}
	if !strings.Contains(uri, "windows-control.teenet.app") {
		t.Errorf("enrolment URI = %q, want the console named in it", uri)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if _, err := svc.CompleteEnrolment(ctx, admin, secret, code); err != nil {
		t.Fatalf("complete enrolment: %v", err)
	}
	admin, err = admins.ByID(ctx, admin.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return svc, admins, admin, password, secret
}

func TestFirstAccountAndEnrolment(t *testing.T) {
	admins := newFakeAdmins()
	svc := New(admins, testKeyring(t), "")
	ctx := context.Background()

	needs, err := svc.NeedsBootstrap(ctx)
	if err != nil || !needs {
		t.Fatalf("NeedsBootstrap = %v (%v), want true on an empty database", needs, err)
	}

	admin, password, err := svc.CreateAccount(ctx, "zhang", "zhang@example.com", repo.RoleAdmin)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(password) < 16 {
		t.Errorf("generated password is %d characters, want a long one", len(password))
	}
	// The password is returned once and never stored: what is in the row is a
	// hash, and it must not be the password itself.
	if strings.Contains(admin.PasswordHash, password) {
		t.Fatal("the password appears in the stored hash")
	}
	if err := VerifyPassword(admin.PasswordHash, password); err != nil {
		t.Fatalf("the generated password does not verify: %v", err)
	}
	if needs, _ := svc.NeedsBootstrap(ctx); needs {
		t.Error("NeedsBootstrap is still true after an account was created")
	}

	// Signing in before enrolment is not a failure -- the password was right.
	// The caller has to send them to enrolment rather than into the console.
	_, _, err = svc.SignIn(ctx, "zhang", password, "", "203.0.113.7", "Firefox")
	if !errors.Is(err, ErrEnrolmentRequired) {
		t.Fatalf("sign-in before enrolment: error = %v, want ErrEnrolmentRequired", err)
	}

	secret, _, err := svc.BeginEnrolment(admin)
	if err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}
	// Nothing is stored until the code proves the authenticator really has the
	// seed: storing it first is how an account ends up with a factor nobody
	// can produce.
	reloaded, _ := admins.ByID(ctx, admin.ID)
	if reloaded.TOTPEnrolled() {
		t.Fatal("the seed was stored before it was confirmed")
	}
	if _, err := svc.CompleteEnrolment(ctx, admin, secret, "000000"); !errors.Is(err, ErrSignInFailed) {
		t.Errorf("enrolment with a wrong code: error = %v, want it refused", err)
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	codes, err := svc.CompleteEnrolment(ctx, admin, secret, code)
	if err != nil {
		t.Fatalf("complete enrolment: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Errorf("got %d recovery codes, want %d", len(codes), RecoveryCodeCount)
	}
	stored, _ := admins.ByID(ctx, admin.ID)
	// The seed is sealed, and the recovery codes are stored only as hashes: a
	// code that can be read out of the database is a second password in plain
	// sight.
	if string(stored.TOTPSecret) == secret {
		t.Error("the authenticator seed is stored in the clear")
	}
	for _, hash := range stored.RecoveryHashes {
		for _, code := range codes {
			if strings.Contains(hash, code) {
				t.Fatal("a recovery code is stored in the clear")
			}
		}
	}
}

func TestSignInNeedsBothFactors(t *testing.T) {
	svc, admins, admin, password, secret := enrolled(t)
	ctx := context.Background()

	code, _ := totp.GenerateCode(secret, time.Now())
	token, signedIn, err := svc.SignIn(ctx, "ZHANG", password, code, "203.0.113.7", "Firefox")
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if token == "" || signedIn.ID != admin.ID {
		t.Fatalf("sign-in returned %q for %s", token, signedIn.ID)
	}

	// The session is found by the cookie, and the stored key is its hash.
	back, err := svc.Session(ctx, token)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if back.ID != admin.ID {
		t.Errorf("session belongs to %s, want %s", back.ID, admin.ID)
	}
	if _, ok := admins.sessions[token]; ok {
		t.Error("the raw cookie is a key in the session store")
	}

	// A right password with no code, or a wrong code, is not a sign-in.
	if _, _, err := svc.SignIn(ctx, "zhang", password, "", "203.0.113.7", ""); !errors.Is(err, ErrSignInFailed) {
		t.Errorf("no code: error = %v, want ErrSignInFailed", err)
	}
	if _, _, err := svc.SignIn(ctx, "zhang", password, "000000", "203.0.113.7", ""); !errors.Is(err, ErrSignInFailed) {
		t.Errorf("wrong code: error = %v, want ErrSignInFailed", err)
	}
	// A wrong password and an unknown user give the same answer: telling
	// somebody which half they got right tells an attacker which accounts
	// exist.
	_, _, wrongPassword := svc.SignIn(ctx, "zhang", "not-the-password", code, "203.0.113.7", "")
	_, _, unknownUser := svc.SignIn(ctx, "nobody", "not-the-password", code, "203.0.113.7", "")
	if wrongPassword == nil || unknownUser == nil || wrongPassword.Error() != unknownUser.Error() {
		t.Errorf("wrong password says %v, unknown user says %v; they must match", wrongPassword, unknownUser)
	}

	if err := svc.SignOut(ctx, token); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	if _, err := svc.Session(ctx, token); !errors.Is(err, ErrSignInFailed) {
		t.Errorf("the session survived signing out: %v", err)
	}
}

func TestARecoveryCodeWorksOnceAndOnlyOnce(t *testing.T) {
	admins := newFakeAdmins()
	svc := New(admins, testKeyring(t), "")
	ctx := context.Background()
	admin, password, err := svc.CreateAccount(ctx, "zhang", "", repo.RoleAdmin)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	secret, _, err := svc.BeginEnrolment(admin)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	code, _ := totp.GenerateCode(secret, time.Now())
	codes, err := svc.CompleteEnrolment(ctx, admin, secret, code)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	// The phone is gone; the recovery code is the way back in.
	if _, _, err := svc.SignIn(ctx, "zhang", password, codes[0], "203.0.113.7", ""); err != nil {
		t.Fatalf("sign in with a recovery code: %v", err)
	}
	// Typed the way people actually type them.
	messy := strings.ToUpper(strings.ReplaceAll(codes[1], "-", " "))
	if _, _, err := svc.SignIn(ctx, "zhang", password, messy, "203.0.113.7", ""); err != nil {
		t.Fatalf("sign in with a differently formatted recovery code: %v", err)
	}
	// A recovery code that still works after being used is a password with
	// extra steps.
	if _, _, err := svc.SignIn(ctx, "zhang", password, codes[0], "203.0.113.7", ""); !errors.Is(err, ErrSignInFailed) {
		t.Errorf("a used recovery code worked again: %v", err)
	}
	stored, _ := admins.ByID(ctx, admin.ID)
	if len(stored.RecoveryHashes) != RecoveryCodeCount-2 {
		t.Errorf("%d recovery codes left, want %d", len(stored.RecoveryHashes), RecoveryCodeCount-2)
	}
}

func TestGuessingIsLockedOut(t *testing.T) {
	svc, _, _, password, secret := enrolled(t)
	ctx := context.Background()

	for range MaxFailuresPerUser {
		if _, _, err := svc.SignIn(ctx, "zhang", "wrong", "000000", "203.0.113.7", ""); !errors.Is(err, ErrSignInFailed) {
			t.Fatalf("during the guesses: error = %v", err)
		}
	}
	// Locked out is deliberately a different answer: somebody who is locked
	// out needs to know to wait rather than keep trying.
	code, _ := totp.GenerateCode(secret, time.Now())
	_, _, err := svc.SignIn(ctx, "zhang", password, code, "203.0.113.7", "")
	if !errors.Is(err, ErrLockedOut) {
		t.Fatalf("after %d failures: error = %v, want ErrLockedOut", MaxFailuresPerUser, err)
	}
	// Even the right password is refused while the lockout holds -- otherwise
	// the lockout only slows down the guess that was going to fail anyway.
	if !errors.Is(err, ErrLockedOut) {
		t.Error("the correct password got through the lockout")
	}
}

func TestADisabledAccountCannotSignIn(t *testing.T) {
	svc, admins, admin, password, secret := enrolled(t)
	ctx := context.Background()
	if _, err := admins.SetDisabled(ctx, admin.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	code, _ := totp.GenerateCode(secret, time.Now())
	if _, _, err := svc.SignIn(ctx, "zhang", password, code, "203.0.113.7", ""); !errors.Is(err, ErrDisabled) {
		t.Errorf("sign-in to a disabled account: error = %v, want ErrDisabled", err)
	}
}

func TestChangingThePasswordEndsEverySession(t *testing.T) {
	svc, _, admin, password, secret := enrolled(t)
	ctx := context.Background()
	code, _ := totp.GenerateCode(secret, time.Now())
	token, _, err := svc.SignIn(ctx, "zhang", password, code, "203.0.113.7", "")
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}

	if err := svc.ChangePassword(ctx, admin, "not-the-password", "a-much-longer-one"); !errors.Is(err, ErrSignInFailed) {
		t.Errorf("changing the password without the current one: error = %v", err)
	}
	if err := svc.ChangePassword(ctx, admin, password, "short"); err == nil {
		t.Error("a five-character password was accepted")
	}
	if err := svc.ChangePassword(ctx, admin, password, "a-much-longer-one"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	// A password change that leaves the old sessions alive does not help the
	// case it is usually made for.
	if _, err := svc.Session(ctx, token); !errors.Is(err, ErrSignInFailed) {
		t.Errorf("the old session survived a password change: %v", err)
	}
}

func TestASessionCannotOutliveItsAbsoluteLimit(t *testing.T) {
	svc, admins, admin, password, secret := enrolled(t)
	ctx := context.Background()
	code, _ := totp.GenerateCode(secret, time.Now())
	token, _, err := svc.SignIn(ctx, "zhang", password, code, "203.0.113.7", "")
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}

	// CreatedAt is filled in by the database, so the fake leaves it zero;
	// measure the limit from now instead.
	session := admins.sessions[string(hashToken(token))]
	if lifetime := time.Until(session.AbsoluteExpiresAt); lifetime > SessionLifetime+time.Minute {
		t.Errorf("absolute lifetime is %v, want about %v", lifetime, SessionLifetime)
	}
	// Activity may push the idle timeout out; it may never push the absolute
	// one.
	for range 3 {
		if _, err := svc.Session(ctx, token); err != nil {
			t.Fatalf("session: %v", err)
		}
	}
	refreshed := admins.sessions[string(hashToken(token))]
	if refreshed.ExpiresAt.After(refreshed.AbsoluteExpiresAt) {
		t.Error("activity pushed the idle timeout past the absolute limit")
	}
	_ = admin
}

func TestTheFirstAccountIsCreatedAtStartup(t *testing.T) {
	admins := newFakeAdmins()
	svc := New(admins, testKeyring(t), "")
	ctx := context.Background()

	// The chicken and egg: nobody can sign in to create the first account, so
	// the console creates it on the first start and prints the password once.
	admin, password, created, err := svc.EnsureFirstAccount(ctx, "", "ops@example.com")
	if err != nil || !created {
		t.Fatalf("EnsureFirstAccount = created %v (%v), want a new account", created, err)
	}
	if !strings.HasPrefix(admin.Username, "admin-") || len(admin.Username) < 10 {
		t.Errorf("generated user name = %q, want something random and not guessable", admin.Username)
	}
	if err := VerifyPassword(admin.PasswordHash, password); err != nil {
		t.Fatalf("the printed password does not work: %v", err)
	}

	// Every start after the first does nothing: a console that mints an
	// account on each restart is a console with a list of forgotten accounts.
	_, password, created, err = svc.EnsureFirstAccount(ctx, "", "")
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if created || password != "" {
		t.Errorf("the second start created another account (password %q)", password)
	}

	// A name can be chosen when somebody wants one.
	if _, err := admins.SetDisabled(ctx, admin.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	named, _, created, err := svc.EnsureFirstAccount(ctx, "Ops", "")
	if err != nil || !created {
		t.Fatalf("with every account disabled: created %v (%v), want a new one", created, err)
	}
	if named.Username != "ops" {
		t.Errorf("user name = %q, want the one asked for", named.Username)
	}
}
