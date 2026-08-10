// Package token issues and verifies the JWT access tokens used by the API.
//
// Tokens are compact JWS strings signed with HMAC-SHA256 (alg "HS256") and
// carry the registered claims sub (the user's UUID), iat, and exp. Nothing
// else goes in the payload: a JWT body is signed, not encrypted, so anything
// added here is readable by whoever holds the token.
package token

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultTTL is how long an issued access token stays valid.
const DefaultTTL = 24 * time.Hour

// signingMethod is the only algorithm this package will sign or accept.
// Pinning it (and passing it to jwt.WithValidMethods on the parse side) is
// what prevents the classic "alg: none" and RS256->HS256 confusion attacks.
var signingMethod = jwt.SigningMethodHS256

// Errors returned by this package. Callers should map all of them to a
// single opaque 401 so a client cannot tell an expired token from a forged
// one.
var (
	// ErrEmptySecret is returned by New when the signing secret is blank.
	ErrEmptySecret = errors.New("token: signing secret is empty")
	// ErrEmptySubject is returned by Issue when given no subject.
	ErrEmptySubject = errors.New("token: subject is empty")
	// ErrInvalidTTL is returned by New for a non-positive lifetime.
	ErrInvalidTTL = errors.New("token: ttl must be positive")
	// ErrInvalid covers every verification failure: malformed input, a bad
	// signature, an unexpected algorithm, or missing claims.
	ErrInvalid = errors.New("token: invalid")
	// ErrExpired means the signature checked out but exp has passed.
	ErrExpired = errors.New("token: expired")
)

// Issuer signs and verifies tokens with one secret. It is safe for
// concurrent use: every field is read-only after New returns.
type Issuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// Option customises an Issuer. Options exist mainly so tests can control
// the clock and lifetime without exporting mutable state.
type Option func(*Issuer)

// WithTTL overrides DefaultTTL.
func WithTTL(d time.Duration) Option {
	return func(i *Issuer) { i.ttl = d }
}

// WithClock overrides time.Now for both signing and verification, so an
// expiry can be exercised in a test without sleeping.
func WithClock(now func() time.Time) Option {
	return func(i *Issuer) { i.now = now }
}

// New builds an Issuer from the configured signing secret.
//
// A blank secret is rejected here rather than at signing time so that a
// misconfigured deployment fails at startup instead of minting tokens that
// anyone can forge.
func New(secret string, opts ...Option) (*Issuer, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, ErrEmptySecret
	}

	i := &Issuer{
		secret: []byte(secret),
		ttl:    DefaultTTL,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(i)
	}
	if i.ttl <= 0 {
		return nil, ErrInvalidTTL
	}
	if i.now == nil {
		i.now = time.Now
	}

	return i, nil
}

// TTL reports the lifetime of tokens this Issuer mints.
func (i *Issuer) TTL() time.Duration {
	if i == nil {
		return 0
	}
	return i.ttl
}

// Issue returns a signed token whose sub claim is subject.
func (i *Issuer) Issue(subject string) (string, error) {
	if i == nil {
		return "", ErrEmptySecret
	}
	if subject == "" {
		return "", ErrEmptySubject
	}

	now := i.now().UTC()

	signed, err := jwt.NewWithClaims(signingMethod, jwt.RegisteredClaims{
		Subject:   subject,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
	}).SignedString(i.secret)
	if err != nil {
		return "", fmt.Errorf("token: sign: %w", err)
	}

	return signed, nil
}

// Subject verifies raw and returns its sub claim.
//
// Verification is delegated to jwt.ParseWithClaims, which checks the
// signature before any claim is acted on. The options are the security
// contract and none of them are optional:
//   - WithValidMethods pins HS256, so a token whose header asks for "none"
//     or an asymmetric algorithm is rejected before the key is consulted.
//   - WithExpirationRequired rejects a token with no exp, which would
//     otherwise be treated as valid forever.
func (i *Issuer) Subject(raw string) (string, error) {
	// A nil Issuer means auth was never wired up. Fail closed rather than
	// panicking a request goroutine.
	if i == nil {
		return "", ErrInvalid
	}

	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return i.secret, nil },
		jwt.WithValidMethods([]string{signingMethod.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return i.now().UTC() }),
	)
	if err != nil {
		// Expiry is reported separately because it is the one failure that
		// is a normal part of a token's life rather than a sign of tampering.
		if errors.Is(err, jwt.ErrTokenExpired) {
			return "", ErrExpired
		}
		return "", ErrInvalid
	}

	// A signed token with an empty sub is not usable as a credential: the
	// middleware would put an empty user id on the context.
	if claims.Subject == "" {
		return "", ErrInvalid
	}

	return claims.Subject, nil
}
