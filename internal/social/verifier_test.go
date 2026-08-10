package social_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tapago/tapago-api/internal/social"
)

// One key for the whole package: generating RSA keys is the slowest thing
// these tests do, and every case wants the same signer anyway.
var (
	signingKeyOnce sync.Once
	signingKey     *rsa.PrivateKey
)

const testKID = "test-key-1"

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	signingKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		signingKey = k
	})
	return signingKey
}

// jwksServer serves the public half of the test key in JWKS form and counts
// how many times it was asked, so caching can be asserted.
func jwksServer(t *testing.T, pub *rsa.PublicKey) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwkFor(pub, testKID)}})
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

func jwkFor(pub *rsa.PublicKey, kid string) map[string]string {
	return map[string]string{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func sign(t *testing.T, claims jwt.MapClaims, kid string) string {
	t.Helper()

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid

	signed, err := tok.SignedString(key(t))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

var testNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return testNow }

func googleClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":            "https://accounts.google.com",
		"aud":            "tapago-ios.apps.googleusercontent.com",
		"sub":            "google-sub-123",
		"email":          "Person@Example.com",
		"email_verified": true,
		"name":           "A Person",
		"iat":            testNow.Add(-time.Minute).Unix(),
		"exp":            testNow.Add(time.Hour).Unix(),
	}
}

func appleClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": "https://appleid.apple.com",
		"aud": "com.tapago.app",
		"sub": "apple-sub-456",
		// Apple sends this claim as a JSON string rather than a boolean.
		"email":          "person@privaterelay.appleid.com",
		"email_verified": "true",
		"iat":            testNow.Add(-time.Minute).Unix(),
		"exp":            testNow.Add(time.Hour).Unix(),
	}
}

func newGoogle(t *testing.T, jwksURL string, audiences ...string) *social.ProviderVerifier {
	t.Helper()
	if len(audiences) == 0 {
		audiences = []string{"tapago-ios.apps.googleusercontent.com"}
	}
	v, err := social.NewGoogle(audiences, social.WithJWKSURL(jwksURL), social.WithClock(fixedClock))
	if err != nil {
		t.Fatalf("NewGoogle: %v", err)
	}
	return v
}

func TestVerifyAcceptsGoogleToken(t *testing.T) {
	srv, _ := jwksServer(t, &key(t).PublicKey)

	identity, err := newGoogle(t, srv.URL).Verify(context.Background(), sign(t, googleClaims(), testKID))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if identity.Provider != social.ProviderGoogle {
		t.Errorf("Provider = %q, want %q", identity.Provider, social.ProviderGoogle)
	}
	if identity.Subject != "google-sub-123" {
		t.Errorf("Subject = %q", identity.Subject)
	}
	// The address must arrive normalised: the users table matches on the
	// lower-cased form.
	if identity.Email != "person@example.com" {
		t.Errorf("Email = %q, want it lower-cased", identity.Email)
	}
	if !identity.EmailVerified {
		t.Error("EmailVerified = false, want true")
	}
	if identity.Name != "A Person" {
		t.Errorf("Name = %q", identity.Name)
	}
}

// Google still mints the bare-host issuer; rejecting it would reject real
// tokens.
func TestVerifyAcceptsBothGoogleIssuerSpellings(t *testing.T) {
	srv, _ := jwksServer(t, &key(t).PublicKey)

	for _, issuer := range []string{"https://accounts.google.com", "accounts.google.com"} {
		t.Run(issuer, func(t *testing.T) {
			claims := googleClaims()
			claims["iss"] = issuer

			if _, err := newGoogle(t, srv.URL).Verify(context.Background(), sign(t, claims, testKID)); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

func TestVerifyAcceptsAppleToken(t *testing.T) {
	srv, _ := jwksServer(t, &key(t).PublicKey)

	v, err := social.NewApple([]string{"com.tapago.app"}, social.WithJWKSURL(srv.URL), social.WithClock(fixedClock))
	if err != nil {
		t.Fatalf("NewApple: %v", err)
	}

	identity, err := v.Verify(context.Background(), sign(t, appleClaims(), testKID))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if identity.Provider != social.ProviderApple {
		t.Errorf("Provider = %q, want %q", identity.Provider, social.ProviderApple)
	}
	// The string form of email_verified must not read as unverified: that
	// would quietly disable account linking for every Apple user.
	if !identity.EmailVerified {
		t.Error(`EmailVerified = false for the string claim "true", want true`)
	}
	// Apple never puts a name in the ID token.
	if identity.Name != "" {
		t.Errorf("Name = %q, want empty", identity.Name)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	srv, _ := jwksServer(t, &key(t).PublicKey)

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	foreign := jwt.NewWithClaims(jwt.SigningMethodRS256, googleClaims())
	foreign.Header["kid"] = testKID
	forged, err := foreign.SignedString(otherKey)
	if err != nil {
		t.Fatalf("sign forged token: %v", err)
	}

	expired := googleClaims()
	expired["exp"] = testNow.Add(-2 * time.Hour).Unix()

	noExp := googleClaims()
	delete(noExp, "exp")

	wrongAudience := googleClaims()
	wrongAudience["aud"] = "some-other-app.apps.googleusercontent.com"

	wrongIssuer := googleClaims()
	wrongIssuer["iss"] = "https://accounts.evil.example"

	noSubject := googleClaims()
	delete(noSubject, "sub")

	// The classic key-confusion attempt: sign with the public key as an HMAC
	// secret and hope the verifier trusts the header's alg.
	hmacToken := jwt.NewWithClaims(jwt.SigningMethodHS256, googleClaims())
	hmacToken.Header["kid"] = testKID
	hmacSigned, err := hmacToken.SignedString([]byte("not-the-key"))
	if err != nil {
		t.Fatalf("sign hmac token: %v", err)
	}

	cases := map[string]string{
		"empty":                   "",
		"not a jwt":               "not-a-jwt",
		"signed by another key":   forged,
		"expired":                 sign(t, expired, testKID),
		"no expiry":               sign(t, noExp, testKID),
		"audience of another app": sign(t, wrongAudience, testKID),
		"unexpected issuer":       sign(t, wrongIssuer, testKID),
		"no subject":              sign(t, noSubject, testKID),
		"unpublished key id":      sign(t, googleClaims(), "rotated-away"),
		"hmac instead of rs256":   hmacSigned,
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newGoogle(t, srv.URL).Verify(context.Background(), raw)
			if !errors.Is(err, social.ErrInvalidToken) {
				t.Fatalf("error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

// A token for a *different* one of our own client ids is still ours.
func TestVerifyAcceptsAnyConfiguredAudience(t *testing.T) {
	srv, _ := jwksServer(t, &key(t).PublicKey)

	claims := googleClaims()
	claims["aud"] = "tapago-android.apps.googleusercontent.com"

	v := newGoogle(t, srv.URL, "tapago-ios.apps.googleusercontent.com", "tapago-android.apps.googleusercontent.com")
	if _, err := v.Verify(context.Background(), sign(t, claims, testKID)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// An unreachable key endpoint means "we could not check", which must be
// distinguishable from "this token is bad" — the handler turns one into a 503
// and the other into a 401.
func TestVerifyReportsUnavailableKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newGoogle(t, srv.URL).Verify(context.Background(), sign(t, googleClaims(), testKID))
	if !errors.Is(err, social.ErrKeysUnavailable) {
		t.Fatalf("error = %v, want ErrKeysUnavailable", err)
	}
	if errors.Is(err, social.ErrInvalidToken) {
		t.Error("a key-fetch failure must not be reported as an invalid token")
	}
}

func TestVerifyCachesKeys(t *testing.T) {
	srv, hits := jwksServer(t, &key(t).PublicKey)

	v := newGoogle(t, srv.URL)
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(context.Background(), sign(t, googleClaims(), testKID)); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("JWKS fetches = %d, want 1: the key set must be cached", got)
	}
}

// An unknown key id must not turn every request into an outbound fetch, or a
// client sending random kids becomes an amplifier against the provider.
func TestVerifyThrottlesRefetchForUnknownKeyID(t *testing.T) {
	srv, hits := jwksServer(t, &key(t).PublicKey)

	v := newGoogle(t, srv.URL)
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(context.Background(), sign(t, googleClaims(), "unknown-kid")); !errors.Is(err, social.ErrInvalidToken) {
			t.Fatalf("error = %v, want ErrInvalidToken", err)
		}
	}

	if got := hits.Load(); got > 1 {
		t.Errorf("JWKS fetches = %d, want at most 1", got)
	}
}

// Key rotation: once the cache expires, a newly published key id resolves.
func TestVerifyPicksUpRotatedKeys(t *testing.T) {
	pub := &key(t).PublicKey
	published := testKID

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwkFor(pub, published)}})
	}))
	defer srv.Close()

	now := testNow
	v, err := social.NewGoogle([]string{"tapago-ios.apps.googleusercontent.com"},
		social.WithJWKSURL(srv.URL),
		social.WithClock(func() time.Time { return now }),
		social.WithKeyTTL(time.Minute),
	)
	if err != nil {
		t.Fatalf("NewGoogle: %v", err)
	}

	if _, err := v.Verify(context.Background(), sign(t, googleClaims(), testKID)); err != nil {
		t.Fatalf("Verify before rotation: %v", err)
	}

	published = "test-key-2"
	now = now.Add(2 * time.Minute) // past both the TTL and the refresh throttle

	claims := googleClaims()
	claims["iat"] = now.Add(-time.Minute).Unix()
	claims["exp"] = now.Add(time.Hour).Unix()

	if _, err := v.Verify(context.Background(), sign(t, claims, "test-key-2")); err != nil {
		t.Fatalf("Verify after rotation: %v", err)
	}
}

// A verifier with no audience would accept tokens minted for any application
// on earth, so it must not be constructible.
func TestNewRejectsEmptyAudience(t *testing.T) {
	for name, audiences := range map[string][]string{
		"nil":    nil,
		"empty":  {},
		"blanks": {"", "   "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := social.NewGoogle(audiences); !errors.Is(err, social.ErrNoAudience) {
				t.Errorf("NewGoogle error = %v, want ErrNoAudience", err)
			}
			if _, err := social.NewApple(audiences); !errors.Is(err, social.ErrNoAudience) {
				t.Errorf("NewApple error = %v, want ErrNoAudience", err)
			}
		})
	}
}
