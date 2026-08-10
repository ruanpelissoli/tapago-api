# internal/token

## Purpose

Issues and verifies the JWT access tokens the API hands out at register and
login and checks on every protected request. `Issuer` is the whole surface:
`Issue(subject) -> string` and `Subject(raw) -> userID`.

## Key decisions

- **`github.com/golang-jwt/jwt/v5`**, mandated by the task spec. An earlier
  draft hand-rolled HS256 signing and parsing; it was replaced for the same
  reason as the password package — this is not a place to be original.
- **The package exposes `Issue`/`Subject`, not JWT types.** No caller sees a
  `jwt.Token` or a claims struct, so swapping the library again (or moving to
  opaque tokens) touches this file only. `internal/middleware` depends on a
  one-method `TokenVerifier` interface, not on this package.
- **`WithClock` instead of sleeping in tests.** Expiry is exercised by moving
  a fake clock past `exp`, which keeps the suite fast and deterministic.
- **Errors are deliberately coarse.** Everything except expiry collapses to
  `ErrInvalid`, so a caller cannot accidentally tell a client whether a token
  was forged, malformed, or signed with the wrong key.

## Business logic

- Claims are exactly `sub` (user UUID), `iat`, `exp` — nothing else. A JWT
  payload is **signed, not encrypted**: anything added here is readable by
  whoever holds the token. Do not put email, name, or roles in it.
- `DefaultTTL` is 24h, fixed by the acceptance criteria.
- A blank secret is rejected by `New`, so a misconfigured deployment fails at
  startup instead of minting forgeable tokens. `cmd/api` calls `New` before
  opening the database for exactly that reason.
- A signed token with an empty `sub` is rejected — it would otherwise put an
  empty user id on the request context.

## Dependencies

`github.com/golang-jwt/jwt/v5`. Imported by `cmd/api` (constructs the
issuer), `internal/router` (passes it to both), and satisfied-by-interface in
`internal/middleware` and `internal/handler/auth`.

## Gotchas

- **The parse options in `Subject` are the security contract, not defaults.**
  `WithValidMethods` pins HS256 — without it a token whose header says
  `alg: none` or an asymmetric algorithm is accepted. `WithExpirationRequired`
  rejects a token with no `exp`, which would otherwise never expire. Removing
  either is an authentication bypass; both have regression tests.
- **A nil `*Issuer` fails closed** (`Subject` returns `ErrInvalid`) rather
  than panicking a request goroutine. The router relies on this so the route
  surface does not change when auth is unconfigured.
- Never log a raw token — it is a bearer credential, equivalent to a password
  for its lifetime.
