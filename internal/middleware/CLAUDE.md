# internal/middleware

## Purpose

Cross-cutting HTTP middleware: structured request logging (`RequestLogger`)
and bearer-token authentication (`RequireAuth`). Rate limiting belongs here
too when it arrives.

## Key decisions

- **Own logger instead of `chimw.Logger`.** chi's logger writes human-shaped
  text; we emit `log/slog` JSON so logs are queryable in aggregation. chi's
  `WrapResponseWriter` is still reused for status/byte capture.
- **Log after the handler returns**, in a `defer`, so status and duration are
  known and a panic is still logged before `Recoverer` handles it.
- **5xx logs at error level.** Alerting can key on level alone.
- **`RequireAuth` takes a `TokenVerifier` interface, not `*token.Issuer`.**
  One method, `Subject(raw) (string, error)`. This package therefore does not
  import `internal/token`, and its tests need no real signing keys.
- **The context key is an unexported empty struct type.** A plain string key
  would be reachable — and forgeable — from any package.

## Business logic

- Exactly one log line per request. `request_id` is included only when
  present — it comes from chi's `RequestID`, which must run first.
- `RequireAuth` is a **route-group** middleware, never global: `/health` and
  the `/auth/*` routes must stay reachable without a token.
- **Every rejection returns the same body and status** — absent header, wrong
  scheme, bad signature, expired token all yield 401 `unauthorized`. Telling
  a caller *why* its token failed only helps someone probing the endpoint.
- A 401 carries `WWW-Authenticate: Bearer`, per RFC 7235.
- `UserID(ctx)` returns `ok == false` for any request that did not pass
  through `RequireAuth`, so a handler can never mistake "no auth" for
  "empty user".
- The `Authorization` value must be exactly two space-separated fields;
  `"Bearer a b"` is rejected rather than having its tail silently dropped.
  The scheme name is compared case-insensitively (RFC 7235).

## Dependencies

`log/slog`, `github.com/go-chi/chi/v5/middleware` (aliased `chimw`), and
`internal/handler` for the error envelope. Imported by `internal/router`.

## Gotchas

- **Only `r.URL.Path` is logged, never `RawQuery`** — query strings can carry
  tokens, emails, or reset codes. The same rule applies to headers: never log
  `Authorization` or `Cookie`, and never log a token even when rejecting it.
- Middleware must pass `ww` (the wrapped writer) to `next`, not the original
  `w`, or status and byte counts read back as zero.
- `RequireAuth` must fail closed if the verifier is nil — `*token.Issuer`
  handles this on its own nil receiver. Do not add a "skip auth when
  unconfigured" branch here.
- Anything added here runs on *every* request, `/health` included, so keep it
  cheap and non-blocking.
