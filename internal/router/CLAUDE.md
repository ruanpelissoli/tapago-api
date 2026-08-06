# internal/router

## Purpose

The one place that maps URLs to handlers and defines the middleware chain.
`New(Deps)` returns a ready `http.Handler`.

## Key decisions

- **chi, not a framework.** chi routers *are* `http.Handler` and its
  middleware are plain `func(http.Handler) http.Handler`, so nothing here
  locks us into a framework's request/response types. Gin/Echo were rejected
  for that reason.
- **Dependencies via a `Deps` struct, not globals.** Adding a dependency is a
  new field, which does not break existing call sites, and tests can pass a
  zero `Deps{}` for routes that need nothing.
- **Handlers don't know their own paths.** Route strings live only here, so
  the full URL surface is greppable from one file.
- **JSON 404 / 405 overrides.** chi's defaults return plain text; clients
  parsing every response as JSON would choke on an error. All responses from
  this API are JSON.

## Business logic

- Middleware order is load-bearing: `RequestID` → `RealIP` →
  `RequestLogger` → `Recoverer`. RequestID must precede the logger for the
  id to appear in logs; Recoverer sits innermost so a panic becomes a logged
  500 instead of a dropped connection.
- `GET /health` is registered without any dependency on `Deps.DB` — see
  `internal/handler/health` for why.

## Dependencies

Imports `internal/handler`, `internal/handler/health`, `internal/middleware`,
and pgx (for the `Deps.DB` type). Imported by `cmd/api`.

## Gotchas

- `Deps.DB` may be nil in tests. A handler that dereferences it must be
  reachable only from routes that genuinely need the database.
- `r.Use` panics if called after a route is registered — keep all `Use` calls
  above the route block.
- Feature routes should be mounted with `r.Route("/v1/...", ...)` subrouters
  so per-feature middleware (e.g. auth) does not leak onto `/health`.
