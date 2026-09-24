// Package auth holds the credential primitives behind the admin UI login:
// password hashing, session token generation, and login attempt throttling.
//
// It deliberately owns no SQL. Persistence lives in internal/storage, so this
// package stays testable without a database and the layering matches the rest
// of the project.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// BcryptCost is the work factor for password hashing. bcrypt's default (10) is
// on the low side for a credential guarding an admin panel; 12 costs roughly
// 250ms per verification, which is negligible at login rates and materially
// slows an offline attack on a stolen hash.
const BcryptCost = 12

// MinPasswordLength is the shortest password accepted. Length is the only
// requirement enforced: composition rules push people toward predictable
// substitutions without adding real entropy.
const MinPasswordLength = 12

// ErrPasswordTooShort is returned by HashPassword for an undersized password.
var ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)

// ErrPasswordTooLong is returned for input past bcrypt's 72-byte limit.
//
// bcrypt silently ignores everything after byte 72. Accepting a longer password
// would mean two different passwords sharing a hash, so reject instead.
var ErrPasswordTooLong = errors.New("password must be at most 72 bytes")

// HashPassword validates and bcrypt-hashes a plaintext password.
func HashPassword(plain string) (string, error) {
	if utf8.RuneCountInString(plain) < MinPasswordLength {
		return "", ErrPasswordTooShort
	}
	if len(plain) > 72 {
		return "", ErrPasswordTooLong
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword reports whether plain matches hash. It is constant-time with
// respect to the password (bcrypt's own comparison), so it leaks nothing about
// how much of a guess was correct.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// DummyHash is a valid bcrypt digest of a random value, used to keep the timing
// of a login against an unknown username indistinguishable from one against a
// known username with the wrong password. Without this, an attacker times the
// response and enumerates valid usernames.
var DummyHash = mustDummyHash()

func mustDummyHash() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: entropy source unavailable: " + err.Error())
	}
	h, err := bcrypt.GenerateFromPassword(b, BcryptCost)
	if err != nil {
		panic("auth: cannot build dummy hash: " + err.Error())
	}
	return string(h)
}

// NewSessionToken mints the value handed to the browser: 32 bytes of entropy,
// base64url-encoded. Returned alongside its storage id (see HashToken).
func NewSessionToken() (token, id string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken derives the database id for a session cookie value.
//
// Only this digest is stored, so a leaked database yields no usable cookies. A
// plain SHA-256 is right here (unlike for passwords): the token is 256 bits of
// uniform entropy, so there is nothing to brute-force and a slow hash would
// only tax every authenticated request.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TokensEqual compares two session tokens in constant time.
func TokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
