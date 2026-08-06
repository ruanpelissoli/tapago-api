# internal/handler/auth

## Purpose

Placeholder for the authentication handlers: registration, login, and token
refresh. Empty by design — the package exists so the layout and import paths
are settled before auth is implemented.

## Key decisions

- Created empty rather than omitted so the router's future mount point and
  this package's import path do not churn later.

## Business logic

None yet. When implementing, the constraints already assumed elsewhere:

- `JWT_SECRET` comes from `internal/config`; make it **required** there
  (it is currently optional) rather than checking it at signing time.
- Password hashing must be bcrypt or argon2id — never SHA-family.
- Login must not reveal whether an email exists: same error, same status,
  same rough timing for unknown-user and wrong-password.
- Auth *enforcement* middleware belongs in `internal/middleware`, not here;
  this package only handles credential exchange.

## Dependencies

Will depend on `internal/handler` (response helpers), `internal/db`, and
`internal/model`. Nothing imports it yet.

## Gotchas

- Never log request bodies from this package — they contain passwords and
  tokens. The shared request logger deliberately omits query strings for the
  same reason.
