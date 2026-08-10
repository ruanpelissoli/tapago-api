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
  in 1–65535; `DATABASE_URL` and `JWT_SECRET` must be non-blank. Downstream
  code can assume a `Config` is valid.

## Business logic

- `PORT` unset or empty → `DefaultPort` (8080). Present but unparseable or
  out of range → error. An explicitly wrong value is a mistake worth failing
  on; an absent one is not.
- `DATABASE_URL` is trimmed and required — whitespace-only counts as missing
  and returns `ErrMissingDatabaseURL` (matchable with `errors.Is`).
- `JWT_SECRET` is trimmed and **required**, returning `ErrMissingJWTSecret`.
  It became required when auth landed. Validating it at startup rather than
  at signing time is deliberate: a process that boots without a secret would
  either mint tokens anyone can forge or fail every authenticated request at
  runtime, and both are worse than refusing to start.

## Dependencies

Standard library only. Imported by `cmd/api`.

## Gotchas

- **`Config` carries two secrets** — `DatabaseURL` and `JWTSecret`. Never log
  a `Config` value, interpolate one into an error message, or add a `String()`
  method to it.
- Tests use `t.Setenv`, which forbids `t.Parallel()` in the same test. Each
  test must set **every** required variable explicitly; inheriting one from
  the developer's shell makes the suite pass or fail depending on the machine.
- Adding a new required variable breaks every existing test that calls
  `Load()`. That is working as intended — the compiler cannot catch a missing
  environment variable, so the test suite is the only place it surfaces.
