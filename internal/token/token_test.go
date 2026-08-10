package token_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tapago/tapago-api/internal/token"
)

const testSecret = "test-secret-that-is-long-enough-to-be-realistic"

func TestNewRejectsBlankSecret(t *testing.T) {
	for name, secret := range map[string]string{"empty": "", "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			if _, err := token.New(secret); !errors.Is(err, token.ErrEmptySecret) {
				t.Fatalf("err = %v, want ErrEmptySecret", err)
			}
		})
	}
}

func TestNewRejectsNonPositiveTTL(t *testing.T) {
	if _, err := token.New(testSecret, token.WithTTL(0)); !errors.Is(err, token.ErrInvalidTTL) {
		t.Fatalf("err = %v, want ErrInvalidTTL", err)
	}
}

func TestIssueThenSubjectRoundTrips(t *testing.T) {
	iss := mustIssuer(t, testSecret)

	const userID = "6b1e4a1e-2c48-4d0b-9a6a-1c3a9d5e7f01"
	raw, err := iss.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := iss.Subject(raw)
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}
	if got != userID {
		t.Errorf("Subject = %q, want %q", got, userID)
	}
}

func TestIssueRejectsEmptySubject(t *testing.T) {
	if _, err := mustIssuer(t, testSecret).Issue(""); !errors.Is(err, token.ErrEmptySubject) {
		t.Fatalf("err = %v, want ErrEmptySubject", err)
	}
}

// The acceptance criteria pin the claim set, so assert on the decoded
// payload rather than only on the round trip.
func TestIssuedTokenCarriesSubExpAndIat(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	iss := mustIssuer(t, testSecret, token.WithClock(func() time.Time { return now }))

	raw, err := iss.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}

	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	decodeSegment(t, parts[0], &header)
	if header.Alg != "HS256" || header.Typ != "JWT" {
		t.Errorf("header = %+v, want alg HS256 / typ JWT", header)
	}

	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
		Iat int64  `json:"iat"`
	}
	decodeSegment(t, parts[1], &claims)

	if claims.Sub != "user-1" {
		t.Errorf("sub = %q, want %q", claims.Sub, "user-1")
	}
	if claims.Iat != now.Unix() {
		t.Errorf("iat = %d, want %d", claims.Iat, now.Unix())
	}
	if want := now.Add(24 * time.Hour).Unix(); claims.Exp != want {
		t.Errorf("exp = %d, want %d (24h after iat)", claims.Exp, want)
	}
}

func TestSubjectRejectsExpiredToken(t *testing.T) {
	clock := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	iss := mustIssuer(t, testSecret,
		token.WithTTL(time.Hour),
		token.WithClock(func() time.Time { return clock }),
	)

	raw, err := iss.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Same secret, clock moved past exp.
	expired := mustIssuer(t, testSecret,
		token.WithTTL(time.Hour),
		token.WithClock(func() time.Time { return clock.Add(2 * time.Hour) }),
	)
	if _, err := expired.Subject(raw); !errors.Is(err, token.ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestSubjectRejectsTokenSignedWithAnotherSecret(t *testing.T) {
	raw, err := mustIssuer(t, "the-real-secret-value-used-in-production").Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := mustIssuer(t, testSecret).Subject(raw); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestSubjectRejectsTamperedPayload(t *testing.T) {
	iss := mustIssuer(t, testSecret)

	raw, err := iss.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(raw, ".")
	forged, err := json.Marshal(map[string]any{
		"sub": "someone-else",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("marshal forged claims: %v", err)
	}
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forged) + "." + parts[2]

	if _, err := iss.Subject(tampered); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// An unsigned or otherwise malformed token must never be accepted, whatever
// the header claims.
func TestSubjectRejectsMalformedTokens(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","exp":9999999999,"iat":1}`))

	for name, raw := range map[string]string{
		"alg none, empty signature": header + "." + payload + ".",
		"alg none, bogus signature": header + "." + payload + ".not-a-signature",
		"missing segment":           header + "." + payload,
		"too many dots":             header + "." + payload + ".sig.extra",
		"not a token":               "definitely-not-a-jwt",
		"empty string":              "",
		"only separators":           "..",
		"bad base64 claim":          header + ".!!!." + payload,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mustIssuer(t, testSecret).Subject(raw); !errors.Is(err, token.ErrInvalid) {
				t.Fatalf("Subject(%q) err = %v, want ErrInvalid", raw, err)
			}
		})
	}
}

// The HS256 pin must hold even when the attacker signs correctly with a
// different algorithm. Here the token is a valid HS512 JWS under the same
// secret; only the algorithm differs.
func TestSubjectRejectsUnexpectedAlgorithm(t *testing.T) {
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.RegisteredClaims{
		Subject:   "user-1",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign HS512: %v", err)
	}

	if _, err := mustIssuer(t, testSecret).Subject(raw); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// A token with no exp would otherwise be valid forever.
func TestSubjectRejectsTokenWithoutExpiry(t *testing.T) {
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:  "user-1",
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := mustIssuer(t, testSecret).Subject(raw); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// A correctly signed token carrying no subject is not a usable credential.
func TestSubjectRejectsEmptySubjectClaim(t *testing.T) {
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := mustIssuer(t, testSecret).Subject(raw); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// A nil Issuer means auth was never wired; it must fail closed, not panic.
func TestNilIssuerFailsClosed(t *testing.T) {
	var iss *token.Issuer
	if _, err := iss.Subject("anything"); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func mustIssuer(t *testing.T, secret string, opts ...token.Option) *token.Issuer {
	t.Helper()
	iss, err := token.New(secret, opts...)
	if err != nil {
		t.Fatalf("token.New: %v", err)
	}
	return iss
}

func decodeSegment(t *testing.T, segment string, dst any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode segment %q: %v", segment, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("unmarshal segment %q: %v", segment, err)
	}
}
