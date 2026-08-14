# cmd/api

## Purpose

The single executable for the API. It owns process lifecycle only: load
config, open the database pool, build the router, serve, drain on signal.

## Key decisions

- **`main` delegates to `run() error`.** `os.Exit` skips deferred functions,
  so all cleanup lives in `run` and only `main` exits. Moving an `os.Exit`
  into `run` would silently leak the connection pool.
- **Fail fast on a bad database.** `db.Connect` pings during startup; any
  failure returns before the listener opens, so the process exits non-zero
  instead of accepting traffic it cannot serve. Do not downgrade this to a
  warning.
- **Dependencies flow downward.** The pool is created here and passed into
  `router.New(router.Deps{...})`. No package-level singletons anywhere.
- **JSON logs via `log/slog`.** The default handler is set before anything
  else so even early failures are structured.

## Business logic

- Exit code 0 only on a clean shutdown; 1 on any startup or serve failure.
- SIGINT/SIGTERM starts a drain with a 15s budget. The shutdown context is
  intentionally built from `context.Background()`, not the signal context —
  the latter is already cancelled, which would abort in-flight requests
  immediately rather than letting them finish.
- Database connect is capped at 10s so an unreachable host cannot hang boot.
- **Optional integrations warn, they do not fail startup.** `socialVerifiers`
  and `mercadoPagoClient` leave an unconfigured provider nil and log a
  warning: a dev box or test stack must still boot without social sign-in or
  payments. The affected routes stay mounted and answer 503, so the gap is
  visible in the logs and to the client rather than as a missing route.

## Dependencies

Depends on `internal/config`, `internal/db`, `internal/router`. Nothing
imports this package.

## Gotchas

- `ListenAndServe` returns `http.ErrServerClosed` on a normal shutdown; that
  is not an error and is filtered out. Removing that check makes every clean
  stop exit 1.
- The `serveErr` channel is buffered (size 1) so the goroutine cannot leak if
  the shutdown path returns before reading from it.
- Server read/write timeouts are set explicitly. A zero-value `http.Server`
  has none, which is a slowloris exposure.
- **`mercadoPagoClient` returns the interface type and only assigns inside the
  success branch.** Returning a nil `*mercadopago.Client` would produce a
  non-nil interface holding a nil pointer; `handler/payment`'s `mp == nil`
  check would not fire and the 503 fallback would become a panic on the first
  saved card. Do not "simplify" it into a single `return client, err`.
