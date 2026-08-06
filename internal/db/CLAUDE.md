# internal/db

## Purpose

Owns the PostgreSQL connection pool. `Connect` is the only way the rest of
the app gets database access.

## Key decisions

- **`pgxpool` (pgx v5) over `database/sql`.** Native protocol support, better
  performance, and real PostgreSQL types (arrays, JSONB, `numeric`) without
  driver-level conversion hacks. Money amounts in particular need `numeric`
  handled properly.
- **No ORM, no query builder.** All SQL is written by hand at the call site
  so query plans are reviewable and nothing is generated behind our backs.
- **Ping at startup.** `pgxpool.NewWithConfig` is lazy and succeeds against a
  host that does not exist; the explicit `Ping` is what actually validates
  credentials and reachability. Removing it moves the failure from boot to
  the first user request.
- **Pool sized in code, not in the URL.** Keeps tuning in one reviewable
  place instead of spread across per-environment connection strings.

## Business logic

- Empty URL → `ErrEmptyDatabaseURL` (matchable with `errors.Is`); this is a
  config bug, distinct from a connection failure.
- A failed ping closes the pool before returning, so a failed `Connect` never
  leaks connections and the caller has nothing to clean up.
- Caller owns the returned pool and must `Close()` it.

## Dependencies

`github.com/jackc/pgx/v5/pgxpool`. Created by `cmd/api`, handed to
`internal/router` via `Deps`.

## Gotchas

- **Never wrap the database URL into an error or log line** — it contains the
  password. The parse-error path deliberately wraps only pgx's message.
- Pass a context with a timeout to `Connect`; the ping will otherwise block
  as long as the OS TCP timeout allows.
- Pool tuning constants (10 max conns) are a placeholder for one API
  instance. Total connections across all instances must stay under the
  server's `max_connections`.
- Tests here never touch a real database — they only cover the pre-network
  validation paths. Anything requiring a live server needs a build tag.
