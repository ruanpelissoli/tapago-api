# internal/handler/health

## Purpose

`GET /health` — the liveness endpoint for load balancers, container
orchestrators, and uptime monitoring.

## Key decisions

- **Liveness, not readiness.** `Check` takes no dependencies and never
  touches the database. If it pinged Postgres, a brief database blip would
  make every replica fail its health check, get restarted, and turn a
  recoverable dependency outage into a total one.
- If dependency probing is ever needed, add a *separate* `/ready` endpoint
  and point only the deployment gate at it — do not change `/health`.

## Business logic

- Response is exactly `{"status":"ok"}` with HTTP 200. The body and status
  are contract: monitoring and deploy gates match on them, so changing the
  shape is a breaking change.

## Dependencies

`internal/handler` for the JSON helper. Wired in `internal/router`.

## Gotchas

- The endpoint is unauthenticated and must stay that way — probes have no
  credentials. Never expose version, config, or dependency detail here.
- It is exempt from nothing: request logging runs on it too, so it appears in
  logs at whatever frequency the probe interval sets.
