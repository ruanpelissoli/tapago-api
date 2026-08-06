# internal/config

## Purpose

Reads and validates every environment variable the API needs, once, at
startup. Produces a `Config` value that is passed explicitly to whoever
needs it.

## Key decisions

- **`os.Getenv` only, no config library.** The surface is three variables;
  a library would add a dependency and indirection for no gain. Revisit only
  if we need layered files or live reload.
- **`Load` returns an error, it does not exit.** The caller decides the
  failure mode, which is also what makes this package testable.
- **Validation happens here, not at use sites.** `PORT` must parse and fall
  in 1–65535; `DATABASE_URL` must be non-blank. Downstream code can assume a
  `Config` is valid.

## Business logic

- `PORT` unset or empty → `DefaultPort` (8080). Present but unparseable or
  out of range → error. An explicitly wrong value is a mistake worth failing
  on; an absent one is not.
- `DATABASE_URL` is trimmed and required — whitespace-only counts as missing
  and returns `ErrMissingDatabaseURL` (matchable with `errors.Is`).
- `JWT_SECRET` is read but **not** required yet, because no code consumes it.
  When auth lands, make it required here rather than checking it at token
  signing time.

## Dependencies

Standard library only. Imported by `cmd/api`.

## Gotchas

- `Config` carries credentials in `DatabaseURL`. Never log a `Config` value
  or interpolate it into an error message.
- Tests use `t.Setenv`, which forbids `t.Parallel()` in the same test.
