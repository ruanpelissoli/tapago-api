# internal/handler

## Purpose

Shared HTTP response helpers (`JSON`, `Error`) plus the subpackages holding
the concrete handlers: `health/`, `auth/` (register, login, social sign-in,
`/me`), `payment/` (saved Mercado Pago cards) and `bet/` (placing a bet and
holding its stake).

## Key decisions

- **Every response goes through `JSON` or `Error`.** One content type, one
  error envelope (`{"error":"..."}`), no hand-rolled `w.Write` in handlers.
- **Marshal before writing headers.** `json.NewEncoder(w).Encode` commits a
  200 and then fails mid-body on an encoding error, producing truncated JSON.
  Buffering first means a marshal failure can still return a clean 500.
- **One subpackage per feature area**, so a feature's handlers, request
  types, and tests live together and the import path names the feature.

## Business logic

- `Error` messages are client-facing. Never pass a wrapped internal error
  into it: SQL text, driver messages, and connection strings must not leak.
  Log the detail with `slog` and return a generic message.
- Handlers validate input at the boundary and return 4xx before touching any
  dependency.

## Dependencies

Standard library only. Every handler subpackage imports it for `JSON`/`Error`;
`internal/router` imports it for the 404/405 responses.

## Gotchas

- `WriteHeader` can only be called once — writing a status after `JSON` has
  run is a no-op plus a runtime warning.
- A failed body write (client disconnected) is logged at debug, not error;
  it is not a server fault and would otherwise be noise.
- Response DTOs belong in the feature subpackage, not in `internal/model` —
  the domain model must not be forced to match a wire format.
