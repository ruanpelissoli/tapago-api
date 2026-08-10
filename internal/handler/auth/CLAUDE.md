# internal/handler/auth

## Purpose

The email/password credential exchange: `POST /auth/register`,
`POST /auth/login`, and the authenticated `GET /me` lookup. Credential
handling only — enforcing auth on other routes is `internal/middleware`.

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

## Dependencies

`internal/handler` (response helpers), `internal/middleware` (reads the user
id off the context), `internal/password`, pgx. Mounted by `internal/router`.

## Gotchas

- **Never log request bodies or decode errors from this package** — both can
  quote the password. `decodeJSON` deliberately logs only that decoding
  failed.
- Request bodies are capped by `http.MaxBytesReader` before validation, so a
  large upload cannot exhaust memory.
- `msgDuplicateEmail` and `msgBadCredentials` are pinned by the acceptance
  criteria; clients match on these strings. Changing the text is a breaking
  change.
- `Me` depends on `RequireAuth` having run. Mounting it outside the protected
  group yields a 401 rather than a panic, but the route would be wrong.
