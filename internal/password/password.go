// Package password hashes and verifies user passwords using bcrypt.
//
// The stored value is bcrypt's own modular-crypt string, e.g.
//
//	$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy
//
// Every parameter needed to verify a hash (version, cost, salt) travels
// inside that string, so raising the cost never invalidates existing rows:
// Verify reads the cost from the stored value, and NeedsRehash tells the
// caller when to re-hash the plaintext it still has in hand during a
// successful login.
package password

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// DefaultCost is the work factor used for new hashes. bcrypt's own default
// (10) is the floor recommended by the OWASP Password Storage Cheat Sheet.
// Raise it, never lower it — existing hashes keep verifying because the cost
// is stored per row.
const DefaultCost = bcrypt.DefaultCost

// MaxLength is the longest password bcrypt can operate on.
//
// This is a hard property of the algorithm, not a policy choice: bcrypt
// derives its key from at most 72 bytes. Callers must reject longer input at
// the API boundary — see ErrTooLong for why truncating instead is unsafe.
const MaxLength = 72

var (
	// ErrMismatch means the password does not match the hash. It is the
	// only outcome a caller should surface to a client, and even then only
	// as a generic "invalid credentials".
	ErrMismatch = errors.New("password: does not match")
	// ErrMalformedHash means the stored value is not a bcrypt hash this
	// package can read. It indicates corrupt data, not a wrong password.
	ErrMalformedHash = errors.New("password: malformed stored hash")
	// ErrTooLong means the plaintext exceeds MaxLength bytes.
	//
	// Older bcrypt releases truncated silently, which is a real security
	// bug: two distinct passwords sharing a 72-byte prefix collapse into the
	// same credential. x/crypto now returns an error instead, and this
	// package surfaces it so callers validate rather than store a hash of a
	// prefix.
	ErrTooLong = fmt.Errorf("password: longer than %d bytes", MaxLength)
)

// Hash derives a new hash for plain using DefaultCost.
func Hash(plain string) (string, error) {
	return HashWith(plain, DefaultCost)
}

// HashWith derives a hash using an explicit cost.
//
// Production code should call Hash; this exists so tests can stay fast
// without weakening the real default.
func HashWith(plain string, cost int) (string, error) {
	if len(plain) > MaxLength {
		return "", ErrTooLong
	}

	digest, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return "", ErrTooLong
		}
		return "", fmt.Errorf("password: generate: %w", err)
	}

	return string(digest), nil
}

// Verify reports whether plain matches the encoded hash.
//
// It returns ErrMismatch for a wrong password and ErrMalformedHash for a
// stored value it cannot parse. Callers must map both to the same client
// response — the difference is an operational signal, not a user-facing one.
func Verify(encoded, plain string) error {
	// bcrypt's comparison truncates at 72 bytes, so without this guard a
	// password of "<72 correct bytes><anything>" would verify against a hash
	// derived from the first 72. Hash rejects such input, so no stored hash
	// can legitimately have come from it.
	if len(plain) > MaxLength {
		return ErrMismatch
	}

	err := bcrypt.CompareHashAndPassword([]byte(encoded), []byte(plain))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return ErrMismatch
	default:
		// Everything else out of bcrypt is a complaint about the stored
		// value itself: ErrHashTooShort, an unrecognised version prefix, or
		// an unparseable cost.
		return ErrMalformedHash
	}
}

// NeedsRehash reports whether encoded was produced with a weaker cost than
// the current default. Call it after a successful Verify, while the
// plaintext is still available.
func NeedsRehash(encoded string) bool {
	cost, err := bcrypt.Cost([]byte(encoded))
	if err != nil {
		// Unreadable: it cannot be left as it is.
		return true
	}
	return cost < DefaultCost
}

// VerifyDummy performs the same work as Verify against a throwaway hash.
//
// Login handlers call this when the email is unknown so that "no such user"
// and "wrong password" take comparable wall-clock time. Skipping bcrypt on
// the unknown-email path turns login into a fast account-enumeration oracle.
func VerifyDummy(plain string) {
	_ = Verify(dummyHash(), plain)
}

// Computed on first use, not at init: a package-level init would add a full
// bcrypt round (~60ms at DefaultCost) to the startup of every binary that
// links this package, tests included.
var dummyHash = sync.OnceValue(func() string {
	h, err := Hash("dummy-value-for-timing")
	if err != nil {
		// Only reachable if the system CSPRNG fails, which is fatal anyway.
		panic("password: " + err.Error())
	}
	return h
})
