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
  parsing every response as JSON would choke on an error.
- **Auth is a `chi.Group`, not a global middleware.** `/health` and the two
  `/auth/*` routes must stay reachable without a token. A global middleware
  with an allow-list of public paths is the kind of thing that silently grows
  a hole; a group makes the protected set explicit and additive.

## Business logic

- Middleware order is load-bearing: `RequestID` → `RealIP` →
  `RequestLogger` → `Recoverer`. RequestID must precede the logger for the
  id to appear in logs; Recoverer sits innermost so a panic becomes a logged
  500 instead of a dropped connection.
- Public routes: `GET /health`, `POST /auth/register`, `POST /auth/login`,
  `POST /auth/google`, `POST /auth/apple` — the `/auth/*` ones are how a
  client obtains a token, so they cannot require one.
- Protected routes (inside `RequireAuth`): `GET /me`. Add future
  authenticated routes to that group, not above it.
- `GET /health` is registered without any dependency on `Deps.DB` — see
  `internal/handler/health` for why.

## Dependencies

Imports `internal/handler`, `internal/handler/auth`, `internal/handler/health`,
`internal/middleware`, `internal/token`, and pgx (for the `Deps.DB` type).
Imported by `cmd/api`.

## Gotchas

- **`Deps.Tokens` may be nil and that must stay safe.** A nil `*token.Issuer`
  fails closed — verification rejects every token — so the route surface is
  identical whether or not auth is configured. Do not "fix" this by skipping
  the route registration; a route that disappears based on config is far
  harder to debug than one that returns 401.
- **`Deps.Social` members may be nil too**, for the same reason: a provider
  with no client id configured keeps its route and answers 503. Do not gate
  route registration on configuration.
- `Deps.DB` may be nil in tests. Routes that dereference it must be reachable
  only from tests that supply one — the auth wiring tests deliberately stay on
  paths that reject before any query runs.
- `r.Use` panics if called after a route is registered — keep all `Use` calls
  above the route block. Inside `r.Group`, `r.Use` applies only to that group.
