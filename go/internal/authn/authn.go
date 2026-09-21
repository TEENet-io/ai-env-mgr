package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

// Sign-in policy. These are the numbers the console runs with; they are here,
// named, rather than spread through the code as literals.
const (
	// SessionIdle is how long a session survives with nobody using it.
	SessionIdle = 8 * time.Hour
	// SessionLifetime is the hard limit, refreshed by nothing. Idle timeout
	// alone would let one sign-in last for ever behind an open tab.
	SessionLifetime = 7 * 24 * time.Hour

	// LockoutWindow and the two thresholds: enough to stop guessing, not so
	// little that a person who fat-fingers their password twice is locked out
	// of their own console.
	LockoutWindow      = 15 * time.Minute
	MaxFailuresPerUser = 8
	MaxFailuresPerIP   = 30

	// RecoveryCodeCount is how many one-time codes enrolment produces. Enough
	// that losing a phone is an inconvenience rather than an incident.
	RecoveryCodeCount = 10
)

// Errors a caller is expected to act on. Everything else is a fault.
var (
	// ErrSignInFailed covers a wrong password, an unknown user and a wrong
	// code alike. Which one it was goes to the attempt log, not to whoever is
	// typing: telling them which half they got right is telling an attacker
	// which accounts exist.
	ErrSignInFailed = errors.New("user name, password or code is wrong")

	// ErrLockedOut means too many failures recently. It is deliberately
	// distinguishable, because a person who is locked out needs to know to
	// wait rather than to keep trying.
	ErrLockedOut = errors.New("too many failed attempts; try again later")

	// ErrEnrolmentRequired means the password was right but the account has no
	// second factor yet. The caller should send them to enrolment rather than
	// into the console.
	ErrEnrolmentRequired = errors.New("this account still has to set up its authenticator")

	// ErrDisabled means the account exists and is switched off.
	ErrDisabled = errors.New("this account is disabled")
)

// Service is the sign-in logic. It holds the account store and the keyring
// that seals TOTP seeds; nothing here writes a plaintext secret anywhere.
type Service struct {
	admins repo.Admins
	ring   secrets.Keyring
	issuer string
	// now is injectable so the tests can move time without sleeping.
	now func() time.Time
}

// New builds a Service. issuer is what appears in the authenticator app -- the
// console's host name is the right answer, because somebody with three of
// these on their phone has to be able to tell them apart.
func New(admins repo.Admins, ring secrets.Keyring, issuer string) *Service {
	if issuer == "" {
		issuer = "windows-control"
	}
	return &Service{admins: admins, ring: ring, issuer: issuer, now: func() time.Time { return time.Now().UTC() }}
}

// NeedsBootstrap reports whether there is no way into the console yet. The web
// layer turns this into a one-time page that creates the first account.
func (s *Service) NeedsBootstrap(ctx context.Context) (bool, error) {
	n, err := s.admins.CountEnabled(ctx)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// CreateAccount adds an administrator with a generated password, which it
// returns exactly once. The password is not stored, logged or recoverable;
// whoever runs this has to pass it on, and the account is expected to change
// it and enrol an authenticator at first sign-in.
func (s *Service) CreateAccount(ctx context.Context, username, email, role string) (repo.Admin, string, error) {
	password, err := GeneratePassword(20)
	if err != nil {
		return repo.Admin{}, "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return repo.Admin{}, "", err
	}
	admin, err := s.admins.Create(ctx, repo.NewAdmin{
		Username: username, Email: email, PasswordHash: hash, Role: role,
	})
	if err != nil {
		return repo.Admin{}, "", err
	}
	return admin, password, nil
}

// EnsureFirstAccount creates an administrator if the console has none, and
// reports the generated password so the caller can print it once.
//
// This is the answer to the chicken and egg: the console cannot be signed in
// to before an account exists, and an account cannot be created through a page
// that requires signing in. It runs at startup, does nothing at all on every
// start after the first, and needs no bootstrap page and no separate tool.
//
// created is false when an account already existed, and then the password is
// empty: there is no way to recover an existing one, by design.
func (s *Service) EnsureFirstAccount(ctx context.Context, username, email string) (admin repo.Admin, password string, created bool, err error) {
	needs, err := s.NeedsBootstrap(ctx)
	if err != nil {
		return repo.Admin{}, "", false, err
	}
	if !needs {
		return repo.Admin{}, "", false, nil
	}
	if username == "" {
		// Random rather than "admin": a guessable name is half of a
		// credential, and this console answers on the public internet.
		suffix, err := randomString(passwordAlphabet, 6)
		if err != nil {
			return repo.Admin{}, "", false, err
		}
		username = "admin-" + strings.ToLower(suffix)
	}
	admin, password, err = s.CreateAccount(ctx, username, email, repo.RoleAdmin)
	if err != nil {
		// Two consoles starting at once: the other one won the race, which
		// means there is an account now and that is all this was for.
		if errors.Is(err, repo.ErrDuplicate) {
			return repo.Admin{}, "", false, nil
		}
		return repo.Admin{}, "", false, err
	}
	return admin, password, true, nil
}

// SignIn checks a password and a six-digit code, and opens a session.
//
// The order matters: the lockout is consulted first, so that guessing costs an
// attacker attempts rather than time; and the password is verified even for an
// unknown user, so that "no such account" does not answer faster than "wrong
// password" and become a way to enumerate names.
func (s *Service) SignIn(ctx context.Context, username, password, code, sourceIP, userAgent string) (string, repo.Admin, error) {
	byUser, byIP, err := s.admins.RecentFailures(ctx, username, sourceIP, LockoutWindow)
	if err != nil {
		return "", repo.Admin{}, err
	}
	if byUser >= MaxFailuresPerUser || byIP >= MaxFailuresPerIP {
		s.record(ctx, username, sourceIP, repo.LoginLocked)
		return "", repo.Admin{}, ErrLockedOut
	}

	admin, err := s.admins.ByUsername(ctx, username)
	if err != nil {
		if !errors.Is(err, repo.ErrNotFound) {
			return "", repo.Admin{}, err
		}
		// Spend the same work as a real verification would, so the answer for
		// an account that does not exist does not come back noticeably sooner.
		_ = VerifyPassword(decoyHash, password)
		s.record(ctx, username, sourceIP, repo.LoginUnknownUser)
		return "", repo.Admin{}, ErrSignInFailed
	}
	if !admin.Enabled() {
		s.record(ctx, username, sourceIP, repo.LoginDisabled)
		return "", repo.Admin{}, ErrDisabled
	}
	if err := VerifyPassword(admin.PasswordHash, password); err != nil {
		if !errors.Is(err, ErrMismatch) {
			return "", repo.Admin{}, err
		}
		s.record(ctx, username, sourceIP, repo.LoginBadPassword)
		return "", repo.Admin{}, ErrSignInFailed
	}

	if !admin.TOTPEnrolled() {
		// The password was right, so this is not a failure to record as one;
		// the account simply cannot finish signing in yet.
		return "", admin, ErrEnrolmentRequired
	}
	ok, err := s.checkSecondFactor(ctx, admin, code)
	if err != nil {
		return "", repo.Admin{}, err
	}
	if !ok {
		s.record(ctx, username, sourceIP, repo.LoginBadTOTP)
		return "", repo.Admin{}, ErrSignInFailed
	}

	token, err := s.openSession(ctx, admin, sourceIP, userAgent)
	if err != nil {
		return "", repo.Admin{}, err
	}
	s.record(ctx, username, sourceIP, repo.LoginOK)
	if err := s.admins.RecordLogin(ctx, admin.ID, s.now()); err != nil {
		return "", repo.Admin{}, err
	}
	return token, admin, nil
}

// decoyHash is a well-formed Argon2id record whose digest matches nothing. It
// is verified against when the account does not exist, purely so that the
// answer costs the same as a real check -- an unknown user that comes back in
// a millisecond is a way to find out which accounts exist.
const decoyHash = "$argon2id$v=19$m=65536,t=3,p=2$c29tZSBzYWx0IHZhbHVl$Zm9yIHRpbWluZyBwdXJwb3NlcyBvbmx5ISEh"

// checkSecondFactor accepts either a current TOTP code or one of the recovery
// codes, which is then consumed.
func (s *Service) checkSecondFactor(ctx context.Context, admin repo.Admin, code string) (bool, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return false, nil
	}

	seed, err := s.ring.Open(ctx, admin.TOTPSecret, admin.TOTPKeyVersion, totpAAD(admin.ID))
	if err != nil {
		// The seed cannot be opened: a wrong master key, or a tampered row.
		// This is a fault, not a wrong code -- answering "try again" would
		// send somebody round a loop that can never succeed.
		return false, fmt.Errorf("open the authenticator seed for %s: %w", admin.Username, err)
	}
	defer wipe(seed)

	if totp.Validate(digitsOnly(code), string(seed)) {
		return true, nil
	}
	return s.consumeRecoveryCode(ctx, admin, code)
}

// consumeRecoveryCode checks a code against the stored hashes and, on a match,
// writes the list back without it. A recovery code that still works after
// being used is a password with extra steps.
func (s *Service) consumeRecoveryCode(ctx context.Context, admin repo.Admin, code string) (bool, error) {
	normalized := normalizeRecoveryCode(code)
	if normalized == "" {
		return false, nil
	}
	for i, hash := range admin.RecoveryHashes {
		if err := VerifyPassword(hash, normalized); err != nil {
			continue
		}
		remaining := make([]string, 0, len(admin.RecoveryHashes)-1)
		remaining = append(remaining, admin.RecoveryHashes[:i]...)
		remaining = append(remaining, admin.RecoveryHashes[i+1:]...)
		if err := s.admins.ReplaceRecoveryHashes(ctx, admin.ID, remaining); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// BeginEnrolment produces a new authenticator seed for an account and the
// otpauth:// URI to show as a QR code.
//
// Nothing is stored yet: the seed becomes the account's second factor only
// when CompleteEnrolment proves that the authenticator actually has it.
// Storing it first is how an account ends up with a factor nobody can produce.
func (s *Service) BeginEnrolment(admin repo.Admin) (secret string, uri string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      s.issuer,
		AccountName: admin.Username,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1, // what every authenticator app supports
	})
	if err != nil {
		return "", "", fmt.Errorf("generate an authenticator seed: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// CompleteEnrolment stores the seed, sealed, once the code proves it arrived,
// and returns the recovery codes -- shown once, stored only as hashes.
func (s *Service) CompleteEnrolment(ctx context.Context, admin repo.Admin, secret, code string) ([]string, error) {
	if !totp.Validate(digitsOnly(code), secret) {
		return nil, ErrSignInFailed
	}
	sealed, keyVersion, err := s.ring.Seal(ctx, []byte(secret), totpAAD(admin.ID))
	if err != nil {
		return nil, fmt.Errorf("seal the authenticator seed: %w", err)
	}

	codes := make([]string, 0, RecoveryCodeCount)
	hashes := make([]string, 0, RecoveryCodeCount)
	for range RecoveryCodeCount {
		code, err := generateRecoveryCode()
		if err != nil {
			return nil, err
		}
		hash, err := HashPassword(normalizeRecoveryCode(code))
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
		hashes = append(hashes, hash)
	}
	if err := s.admins.SetTOTP(ctx, admin.ID, sealed, keyVersion, hashes); err != nil {
		return nil, err
	}
	// Enrolling changes what it takes to sign in, so anything already signed
	// in as this account is ended.
	if _, err := s.admins.DeleteSessionsFor(ctx, admin.ID); err != nil {
		return nil, err
	}
	return codes, nil
}

// ChangePassword sets a new password and ends every session of that account,
// including the one asking. A password change that leaves the old sessions
// alive does not help the case it is usually made for.
func (s *Service) ChangePassword(ctx context.Context, admin repo.Admin, current, next string) error {
	if err := VerifyPassword(admin.PasswordHash, current); err != nil {
		if errors.Is(err, ErrMismatch) {
			return ErrSignInFailed
		}
		return err
	}
	if len(next) < 12 {
		return errors.New("the new password must be at least 12 characters")
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.admins.SetPasswordHash(ctx, admin.ID, hash); err != nil {
		return err
	}
	_, err = s.admins.DeleteSessionsFor(ctx, admin.ID)
	return err
}

// Session returns the account behind a session cookie and refreshes its idle
// timeout. ErrSignInFailed if the cookie is unknown, expired, or belongs to an
// account that has since been disabled.
func (s *Service) Session(ctx context.Context, token string) (repo.Admin, error) {
	if token == "" {
		return repo.Admin{}, ErrSignInFailed
	}
	sum := hashToken(token)
	_, admin, err := s.admins.SessionByToken(ctx, sum)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return repo.Admin{}, ErrSignInFailed
		}
		return repo.Admin{}, err
	}
	if err := s.admins.TouchSession(ctx, sum, s.now().Add(SessionIdle)); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return repo.Admin{}, ErrSignInFailed
		}
		return repo.Admin{}, err
	}
	return admin, nil
}

// SignOut ends one session.
func (s *Service) SignOut(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.admins.DeleteSession(ctx, hashToken(token))
}

func (s *Service) openSession(ctx context.Context, admin repo.Admin, sourceIP, userAgent string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	err := s.admins.CreateSession(ctx, repo.Session{
		TokenSHA256:       hashToken(token),
		PrincipalID:       admin.ID,
		ExpiresAt:         now.Add(SessionIdle),
		AbsoluteExpiresAt: now.Add(SessionLifetime),
		CreatedIP:         sourceIP,
		UserAgent:         userAgent,
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// record writes the attempt and deliberately swallows its error: failing to
// write the log must not turn a successful sign-in into a failed one, nor
// leak, through a different error, which accounts exist.
func (s *Service) record(ctx context.Context, username, sourceIP, outcome string) {
	_ = s.admins.RecordAttempt(ctx, repo.LoginAttempt{
		Username: username, SourceIP: sourceIP, At: s.now(), Outcome: outcome,
	})
}

// hashToken is what the database stores instead of the cookie. A dump, a
// backup or a slow query log must not hand anybody a working session.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// totpAAD binds a sealed seed to the row it belongs to, so a ciphertext copied
// into another account's row will not open.
func totpAAD(adminID string) string { return TOTPAAD(adminID) }

// TOTPAAD is the associated data an administrator's sealed authenticator
// seed is bound to; exported for the re-keying command.
func TOTPAAD(adminID string) string {
	return secrets.AAD("admin_principals", adminID, "totp_seed")
}

// recoveryAlphabet has no vowels -- a recovery code should not be able to
// spell anything -- and none of the characters people confuse when copying.
const recoveryAlphabet = "bcdfghjkmnpqrstvwxz23456789"

func generateRecoveryCode() (string, error) {
	// Five groups of five: 25 characters over a 27-character alphabet, about
	// 118 bits. Grouped because people read these aloud and type them in.
	var groups []string
	for range 5 {
		group, err := randomString(recoveryAlphabet, 5)
		if err != nil {
			return "", err
		}
		groups = append(groups, group)
	}
	return strings.Join(groups, "-"), nil
}

// normalizeRecoveryCode makes the comparison forgiving of how it was typed --
// case, spaces, the dashes -- without making it forgiving of the code itself.
func normalizeRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(code) {
		if strings.ContainsRune(recoveryAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// digitsOnly strips the spaces some authenticators put in the middle of the
// six digits, which people paste along with the code.
func digitsOnly(code string) string {
	var b strings.Builder
	for _, r := range code {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wipe clears a decrypted secret once it has been used. Go can copy a slice
// out from under this, so it is a reduction in exposure rather than a
// guarantee -- worth doing, not worth relying on.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
