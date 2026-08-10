# internal/password

## Purpose

Hashes and verifies user passwords with bcrypt. The only package that
touches plaintext passwords; handlers pass plaintext in and store the
returned string, never the other way round.

## Key decisions

- **bcrypt via `golang.org/x/crypto/bcrypt`**, mandated by the task spec. An
  earlier draft of this package hand-rolled PBKDF2-SHA256; it was replaced
  because hand-written auth crypto is the last place a project should be
  original, and the acceptance criteria named bcrypt explicitly.
- **The stored string is bcrypt's own modular-crypt form** (`$2a$10$...`).
  Version, cost, and salt all travel inside it, so raising `DefaultCost`
  never invalidates existing rows.
- **`HashWith(plain, cost)` exists for tests only.** Tests run at
  `bcrypt.MinCost`; bcrypt's cost is exponential and `DefaultCost` in a test
  suite is seconds of pure waiting. Production code calls `Hash`.
- **`Verify` collapses bcrypt's error set into two outcomes** — `ErrMismatch`
  (wrong password) and `ErrMalformedHash` (corrupt row). Callers must return
  the same generic response for both; the distinction is for logs only.

## Business logic

- **72 bytes is a hard ceiling** (`MaxLength`). bcrypt derives its key from at
  most 72 bytes. `Hash` returns `ErrTooLong` above that rather than storing a
  hash of a prefix, and callers must validate at the API boundary — the
  register handler does.
- **`Verify` also length-checks, and this is load-bearing.** bcrypt's
  comparison silently truncates, so without the guard a login of
  `<72 correct bytes><any garbage>` would authenticate as the real user.
  `TestVerifyDoesNotMatchOnTruncatedPrefix` pins this. Do not remove it.
- `NeedsRehash` reports a stored cost below `DefaultCost`. Call it after a
  successful `Verify`, while the plaintext is still in hand.
- `VerifyDummy` burns a real bcrypt round against a throwaway hash. The login
  handler calls it when the email is unknown so that "no such user" and
  "wrong password" take comparable time — skipping it turns login into a fast
  account-enumeration oracle.

## Dependencies

`golang.org/x/crypto/bcrypt` only. Imported by `internal/handler/auth`.

## Gotchas

- **Never log, wrap, or return the plaintext or the hash.** Error values here
  deliberately carry neither.
- `DefaultCost` is a floor to raise, never lower. Raising it is safe for
  existing rows; lowering it silently weakens every new one.
- The dummy hash is built lazily via `sync.OnceValue`. Moving it to `init`
  adds a full bcrypt round (~60ms) to the startup of every binary and test
  that links this package.
- `MaxLength` is bytes, not runes — multi-byte UTF-8 passwords hit the limit
  sooner than their character count suggests. `len(string)` in Go is already
  bytes, which is what the check wants.
