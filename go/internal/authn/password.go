// Package authn is how somebody signs in to the console: a password, a
// six-digit code, a session cookie, and the rate limiting around them.
//
// It replaces "paste an OSS AccessKey into the login form". A session must not
// carry cloud credentials -- the server has its own identity now -- and taking
// one person's access away must not mean rotating a key everybody shares.
package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. Memory is the one that matters against a GPU: a hash
// that needs 64 MB per guess is a hash somebody cannot try billions of times
// in parallel on hardware built for arithmetic.
//
// They are stored in the encoded hash, so raising them later does not
// invalidate anything: an old hash still verifies with its own parameters, and
// NeedsRehash says when to write a new one.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonKeyLen  = 32
	argonSaltLen = 16
)

// argonThreads is bounded so that a machine with many cores does not produce
// hashes another machine cannot verify at the same cost.
func argonThreads() uint8 {
	if runtime.NumCPU() < 2 {
		return 1
	}
	return 2
}

// ErrMismatch means the password or code is wrong. It is deliberately the same
// error for every reason: whoever is guessing must not learn which half they
// got right.
var ErrMismatch = errors.New("does not match")

// HashPassword returns an encoded Argon2id hash, in the usual
// $argon2id$v=19$m=...,t=...,p=...$salt$hash form. The parameters travel with
// the hash so they can be raised later.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("hash password: the password is empty")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	threads := argonThreads()
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword checks a password against an encoded hash.
//
// The comparison is constant-time. A timing difference on a password hash is
// not the easiest attack in the world, but it is free to avoid.
func VerifyPassword(encoded, password string) error {
	params, salt, want, err := parseHash(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash was made with weaker parameters
// than the ones in use now, so it can be replaced at the next successful
// sign-in -- the one moment the plaintext is available.
func NeedsRehash(encoded string) bool {
	params, _, _, err := parseHash(encoded)
	if err != nil {
		return true
	}
	return params.memory < argonMemory || params.time < argonTime
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func parseHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, fmt.Errorf("stored password hash is not in the expected format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, fmt.Errorf("stored password hash has an unsupported version")
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("stored password hash has unreadable parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("stored password hash has an unreadable salt")
	}
	sum, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(sum) == 0 {
		return argonParams{}, nil, nil, fmt.Errorf("stored password hash is unreadable")
	}
	return p, salt, sum, nil
}

// GeneratePassword returns a random password for a new account.
//
// The alphabet leaves out the characters people mistake for each other when
// reading a password off a screen -- 0/O, 1/l/I -- because this one is going
// to be read off a screen and typed in somewhere else at least once.
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func GeneratePassword(length int) (string, error) {
	if length < 12 {
		length = 20
	}
	return randomString(passwordAlphabet, length)
}

func randomString(alphabet string, length int) (string, error) {
	// Rejection sampling, so every character is equally likely: taking a
	// random byte modulo the alphabet size favours the first few characters,
	// which is a quiet way to lose entropy.
	max := byte(256 - (256 % len(alphabet)))
	out := make([]byte, 0, length)
	buf := make([]byte, length)
	for len(out) < length {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate random string: %w", err)
		}
		for _, b := range buf {
			if b < max {
				out = append(out, alphabet[int(b)%len(alphabet)])
				if len(out) == length {
					break
				}
			}
		}
	}
	return string(out), nil
}
