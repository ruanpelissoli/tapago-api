package password_test

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/tapago/tapago-api/internal/password"
)

// Cheap on purpose: these tests exercise behaviour, not the work factor.
// bcrypt cost is exponential, so MinCost keeps the suite fast.
const testCost = bcrypt.MinCost

func TestHashThenVerify(t *testing.T) {
	const plain = "correct horse battery"

	encoded, err := password.HashWith(plain, testCost)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}
	if err := password.Verify(encoded, plain); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	encoded, err := password.HashWith("the right one", testCost)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}

	for name, wrong := range map[string]string{
		"different": "the wrong one",
		"empty":     "",
		"prefix":    "the right on",
		"suffix":    "the right ones",
	} {
		t.Run(name, func(t *testing.T) {
			if err := password.Verify(encoded, wrong); !errors.Is(err, password.ErrMismatch) {
				t.Fatalf("err = %v, want ErrMismatch", err)
			}
		})
	}
}

// A shared salt would let one rainbow table cover every account.
func TestHashIsSaltedPerCall(t *testing.T) {
	const plain = "same password"

	first, err := password.HashWith(plain, testCost)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}
	second, err := password.HashWith(plain, testCost)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}

	if first == second {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
	if err := password.Verify(second, plain); err != nil {
		t.Fatalf("Verify against second hash: %v", err)
	}
}

func TestEncodedHashNeverContainsPlaintext(t *testing.T) {
	const plain = "hunter2-is-a-terrible-password"

	encoded, err := password.HashWith(plain, testCost)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}
	if strings.Contains(encoded, plain) {
		t.Fatalf("encoded hash %q contains the plaintext", encoded)
	}
	// bcrypt's modular-crypt form: $2<minor>$<cost>$<22-char salt><31-char digest>.
	if !strings.HasPrefix(encoded, "$2") {
		t.Errorf("encoded hash %q is not in bcrypt format", encoded)
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	for name, encoded := range map[string]string{
		"empty":          "",
		"not a hash":     "not-a-hash",
		"truncated":      "$2a$10$tooshort",
		"unknown scheme": "pbkdf2-sha256$210000$c2FsdA$a2V5",
		"bad cost":       "$2a$zz$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
	} {
		t.Run(name, func(t *testing.T) {
			if err := password.Verify(encoded, "anything"); !errors.Is(err, password.ErrMalformedHash) {
				t.Fatalf("err = %v, want ErrMalformedHash", err)
			}
		})
	}
}

// bcrypt derives its key from at most 72 bytes. Accepting longer input would
// mean storing a hash of a prefix, so it must be rejected outright.
func TestHashRejectsOverlongPassword(t *testing.T) {
	tooLong := strings.Repeat("a", password.MaxLength+1)

	if _, err := password.HashWith(tooLong, testCost); !errors.Is(err, password.ErrTooLong) {
		t.Fatalf("err = %v, want ErrTooLong", err)
	}

	// Exactly at the limit must still work.
	if _, err := password.HashWith(strings.Repeat("a", password.MaxLength), testCost); err != nil {
		t.Fatalf("HashWith at MaxLength: %v", err)
	}
}

// The truncation trap: without an explicit guard, bcrypt would report a
// match for a password that merely shares its first 72 bytes with the real
// one. This is the security property that makes the length check necessary.
func TestVerifyDoesNotMatchOnTruncatedPrefix(t *testing.T) {
	atLimit := strings.Repeat("a", password.MaxLength)

	encoded, err := password.HashWith(atLimit, testCost)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}

	if err := password.Verify(encoded, atLimit+"extra"); !errors.Is(err, password.ErrMismatch) {
		t.Fatalf("err = %v, want ErrMismatch for a 72-byte prefix collision", err)
	}
}

func TestNeedsRehash(t *testing.T) {
	weak, err := password.HashWith("x", testCost)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}
	if !password.NeedsRehash(weak) {
		t.Error("NeedsRehash(weak) = false, want true")
	}
	if !password.NeedsRehash("garbage") {
		t.Error("NeedsRehash(garbage) = false, want true")
	}

	current, err := password.HashWith("x", password.DefaultCost)
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}
	if password.NeedsRehash(current) {
		t.Error("NeedsRehash(current) = true, want false")
	}
}

// VerifyDummy exists purely for its timing; assert only that it is safe to
// call and reports nothing about the input.
func TestVerifyDummyDoesNotPanic(t *testing.T) {
	password.VerifyDummy("")
	password.VerifyDummy("anything at all")
	password.VerifyDummy(strings.Repeat("a", password.MaxLength+1))
}
