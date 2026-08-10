# internal/social

## Purpose

Verifies the Google and Apple ID tokens the mobile app forwards after its
SDK has completed the OAuth flow. One question only: is this token really
from the provider, still valid, and issued for *our* app? Account creation
and our own JWT happen above this layer.

## Key decisions

- **Hand-rolled JWKS verification instead of `google.golang.org/api/idtoken`.**
  That module pulls in gRPC, OpenTelemetry and protobuf (~20 transitive
  modules) to do a signature check against a published key set, and covers
  Google only — Apple would still need writing. One `ProviderVerifier` serves
  both: same primitives (RS256, a JWKS endpoint, standard OIDC claims), and
  the only new dependency is the JWT library the API already used. This is
  the "or equivalent" the task allowed.
- **The audience is mandatory at construction.** `NewGoogle`/`NewApple`
  return `ErrNoAudience` with no client ids rather than accepting any `aud`.
  A verifier without an audience accepts tokens minted for *any* app on
  earth, which is a full account takeover — a developer could have a user
  sign in to their app and replay the token here.
- **`ErrKeysUnavailable` is separate from `ErrInvalidToken`.** "We could not
  reach the provider" is not "your token is bad": the handler turns the first
  into 503 and the second into 401. Folding them together would tell apps to
  discard valid credentials during a provider outage.
- **Key cache with a refresh throttle.** Keys are cached for an hour, and an
  unrecognised `kid` triggers at most one refetch per minute. Without the
  throttle, a client sending random `kid` headers turns every request into an
  outbound call — an amplifier against the provider and us.

## Business logic

- Algorithm is pinned to **RS256**. Trusting the header's `alg` is what
  enables "alg: none" and RS256→HS256 key confusion.
- `exp` is required; 60s of clock skew is tolerated on `exp`/`iat`.
- Issuer: Apple `https://appleid.apple.com`; Google accepts **both**
  `https://accounts.google.com` and the bare `accounts.google.com` — it still
  mints the second form, and rejecting it rejects real tokens.
- `email_verified` may arrive as the JSON string `"true"` (Apple) or a real
  boolean (Google); `flexibleBool` handles both. A plain `bool` field would
  silently read the string form as unverified and disable account linking for
  every Apple user.
- `Identity` guarantees only `Subject`. Apple never sends a name in the ID
  token (it comes once, in the authorization response), and the email may be
  a private-relay address or absent.
- A missing `kid` is rejected outright — validating against every published
  key would let a token be verified by a key its header never named.

## Dependencies

`github.com/golang-jwt/jwt/v5` and the standard library. Nothing else in the
repo is imported. Used by `internal/handler/auth` through the narrower
`auth.SocialVerifier` interface.

## Gotchas

- **`WithJWKSURL` exists for tests.** Pointing production at another host
  means trusting whoever runs it to mint identities.
- `Verify` performs a network call on a cold or expired cache, inside the
  user's request. The context is honoured and the client has a 10s timeout.
- The key-set mutex is held across the HTTP fetch. That is deliberate —
  concurrent misses collapse into one request — but it means a slow provider
  serialises sign-ins for that provider until the fetch finishes.
- Errors wrap a reason for logs; handlers must never forward it to a client.
