// Package social verifies the identity tokens ("ID tokens") that Google and
// Apple issue to a mobile app after the user signs in.
//
// There is no OAuth redirect flow here. The mobile SDK performs the whole
// OAuth dance and hands the app a signed JWT; the app forwards it and this
// package answers one question: is this token genuinely from the provider,
// still valid, and issued for *our* client? Everything else — creating or
// linking the account, minting our own token — happens above this layer.
//
// Verification is deliberately implemented on top of the JWT library the API
// already depends on plus a small JWKS client, rather than pulling in
// google.golang.org/api/idtoken. That package drags in gRPC, OpenTelemetry
// and protobuf for what is, on this code path, a signature check against a
// published key set — and it solves Google only, leaving Apple to be written
// by hand anyway. One shared implementation covers both providers: they use
// the same primitives (RS256, a JWKS endpoint, the standard OIDC claims).
package social

import (
	"context"
	"errors"
)

// Provider names, used for logging and to pick the column the account is
// linked on.
const (
	ProviderGoogle = "google"
	ProviderApple  = "apple"
)

// Identity is what a verified ID token asserts about the person signing in.
//
// Only Subject is guaranteed: Apple omits the name entirely (it is delivered
// once, in the authorization response, not in the token), and an email can be
// absent or unverified. Callers must decide what to do with a thin identity;
// this package does not invent values.
type Identity struct {
	// Provider is ProviderGoogle or ProviderApple.
	Provider string
	// Subject is the provider's stable, opaque user id (the "sub" claim).
	// For Apple it is scoped to the developer team, for Google it is global.
	// It is the value an account is linked on.
	Subject string
	// Email is lower-cased and trimmed, or empty if the token carries none.
	Email string
	// EmailVerified reports the provider's own claim about Email. Treating
	// an unverified address as proof of ownership would let anyone who can
	// set that address on a provider account take over the matching local
	// one, so callers must check this before matching by email.
	EmailVerified bool
	// Name is the display name from the token, or empty.
	Name string
}

// Verifier turns a raw ID token into a verified Identity.
//
// Implementations must reject anything they cannot fully verify and must not
// return a partially validated Identity alongside an error.
type Verifier interface {
	Verify(ctx context.Context, idToken string) (Identity, error)
}

var (
	// ErrInvalidToken covers every reason a token is unacceptable:
	// malformed, wrong algorithm, bad signature, expired, wrong issuer,
	// wrong audience, or missing a subject. Callers map all of them to one
	// opaque 401 — telling a client which check failed only helps someone
	// probing the endpoint.
	ErrInvalidToken = errors.New("social: invalid identity token")

	// ErrKeysUnavailable means the provider's public keys could not be
	// fetched, so the token could be neither accepted nor rejected. This is
	// our outage (or theirs), not a bad credential, and must not be reported
	// to the client as a rejected token.
	ErrKeysUnavailable = errors.New("social: identity provider keys unavailable")

	// ErrNoAudience is returned by the constructors when no client id was
	// configured. A verifier that accepts any audience accepts tokens minted
	// for a different app entirely, which is a full account takeover: any
	// developer could have a user sign in to their app and replay the token
	// here. Failing at construction keeps that from ever being a runtime
	// decision.
	ErrNoAudience = errors.New("social: at least one client id is required")
)
