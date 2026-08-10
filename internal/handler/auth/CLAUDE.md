# internal/handler/auth

## Purpose

The credential exchange: `POST /auth/register`, `POST /auth/login`,
`POST /auth/google`, `POST /auth/apple`, and the authenticated `GET /me`
lookup. Credential handling only — enforcing auth on other routes is
`internal/middleware`.

## Key decisions

- **Depends on a one-method `DB` interface, not `*pgxpool.Pool`.** That is
  what lets these handlers be tested without a live database. Same for
  `TokenIssuer`.
- **`userResponse` has no password field at all**, which is stronger than
  `json:"-"`: there is nothing to accidentally un-hide. The domain `User` in
  `internal/model` is never serialised directly.
- **Duplicate email is detected from the unique-violation SQLSTATE (23505),
  not a pre-flight SELECT.** The race between "does this email exist?" and
  "insert it" cannot be closed by checking first, so the unique index is the
  source of truth. A check-then-insert version returns 500 under concurrency.
- **Emails are normalised (lower-cased, trimmed) before storage**, so the
  plain unique index gives the case-insensitivity users expect without the
  `citext` extension.
- **Social sign-in is one flow with a per-provider verifier** (`social.go`).
  Google and Apple differ only in which `SocialVerifier` runs and which column
  the account links on; sharing `socialLogin` keeps the two from drifting.
  Account resolution itself lives in `model.UpsertSocialUser`.
- **A nil verifier keeps its route mounted.** A route that appears or
  disappears with the environment is far harder to debug than one returning
  503 — same reasoning as the nil `token.Issuer`.

## Business logic

- Register returns **201**; login returns **200**. Both return the same
  `{token, user{id,email,name}}` shape.
- Duplicate email → **409** `{"error":"email already registered"}`.
  Bad credentials → **401** `{"error":"invalid credentials"}`.
- **Login must stay an opaque single outcome.** Unknown email calls
  `password.VerifyDummy` so it costs the same as a wrong password — skipping
  it turns login into a fast account-enumeration oracle. An over-long
  password returns 401, not 400, for the same reason.
- **Password length is 8–72 bytes.** The upper bound is `password.MaxLength`,
  which is bcrypt's own limit, *not* a policy number — bcrypt errors above 72
  bytes, so without this check registration would 500 instead of 400. Do not
  raise it without changing the hashing scheme.
- `Me` re-checks that the token subject is a well-formed UUID before it
  reaches Postgres; a malformed value would otherwise become an invalid uuid
  cast (SQLSTATE 22P02) and surface as a 500 rather than a 401.
- A valid token for a deleted user is **401**, not 404 — the credential is
  unusable, and 404 would confirm the account once existed.
- Social sign-in returns **200** whether the account was created or already
  existed: the client cannot act on the difference, and a 201 would leak
  whether an address was registered to anyone able to mint a token for it.
- **`{"error":"invalid social token"}` with 401 means, and only means, the
  provider rejected the token.** A missing `id_token` is 400 (a client bug,
  not a bad credential); an unconfigured provider or an unreachable JWKS
  endpoint is 503 — answering 401 there would send apps into a
  re-authentication loop over a credential that is probably fine.
- **An unverified email is dropped before the upsert.** Otherwise anyone who
  can set a victim's address on a provider account would be handed the
  matching local account. That leaves linking by provider subject, which is
  always safe; a first sign-in with no verified email is 401.
- A password login against a social-only account (NULL `password_hash`) runs
  `password.VerifyDummy` and returns the same 401 as a wrong password, so
  "this address signs in with Google" is not observable from timing.

## Dependencies

`internal/handler` (response helpers), `internal/middleware` (reads the user
id off the context), `internal/password`, `internal/model` (social upsert),
`internal/social` (identity types), pgx. Mounted by `internal/router`.

## Gotchas

- **Never log request bodies or decode errors from this package** — both can
  quote the password. `decodeJSON` deliberately logs only that decoding
  failed.
- Request bodies are capped by `http.MaxBytesReader` before validation, so a
  large upload cannot exhaust memory.
- `msgDuplicateEmail`, `msgBadCredentials` and `msgInvalidSocialToken` are
  pinned by the acceptance criteria; clients match on these strings. Changing
  the text is a breaking change.
- `Me` depends on `RequireAuth` having run. Mounting it outside the protected
  group yields a 401 rather than a panic, but the route would be wrong.
