# internal/middleware

## Purpose

Cross-cutting HTTP middleware. Today: structured request logging. Auth
enforcement and rate limiting belong here too when they arrive.

## Key decisions

- **Own logger instead of `chimw.Logger`.** chi's logger writes human-shaped
  text; we emit `log/slog` JSON so logs are queryable in aggregation. chi's
  `WrapResponseWriter` is still reused for status/byte capture — no reason to
  reimplement it.
- **Log after the handler returns**, in a `defer`, so status and duration are
  known and a panic is still logged before `Recoverer` handles it.
- **5xx logs at error level.** Alerting can key on level alone instead of
  parsing the status field.

## Business logic

- Exactly one log line per request.
- `request_id` is included only when present — it comes from chi's
  `RequestID` middleware, which must run first (wired in `internal/router`).

## Dependencies

`log/slog` and `github.com/go-chi/chi/v5/middleware` (aliased `chimw` to
avoid the name clash with this package). Imported by `internal/router`.

## Gotchas

- **Only `r.URL.Path` is logged, never `RawQuery`** — query strings can carry
  tokens, emails, or reset codes. Do not "improve" this by logging the full
  URL. The same rule applies to headers: never log `Authorization` or
  `Cookie`.
- Middleware must pass `ww` (the wrapped writer) to `next`, not the original
  `w`, or status and byte counts read back as zero.
- Anything added here runs on *every* request, `/health` included, so keep it
  cheap and non-blocking.
