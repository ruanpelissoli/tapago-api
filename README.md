# tapago-api

HTTP API for tapago, written in Go with the standard library plus
[chi](https://github.com/go-chi/chi) for routing and
[pgx](https://github.com/jackc/pgx) for PostgreSQL.

## Requirements

- Go 1.25 or newer
- A reachable PostgreSQL instance

## Running locally

```bash
cp .env.example .env      # then edit DATABASE_URL / JWT_SECRET
set -a && . ./.env && set +a
go run ./cmd/api
```

The server refuses to start — non-zero exit, single error log line — if
`DATABASE_URL` is unset or the database cannot be pinged. That is deliberate:
a process that boots without its database only fails later, in front of users.

Check it is up:

```bash
curl -i localhost:8080/health
# HTTP/1.1 200 OK
# {"status":"ok"}
```

## Configuration

All configuration comes from environment variables; see `.env.example`.

| Variable                   | Required | Default | Purpose                                       |
| -------------------------- | -------- | ------- | --------------------------------------------- |
| `PORT`                     | no       | `8080`  | HTTP listen port                              |
| `DATABASE_URL`             | yes      | —       | PostgreSQL connection string                  |
| `JWT_SECRET`               | no\*     | —       | Signs access tokens (\*required by auth)      |
| `GOOGLE_CLIENT_IDS`        | no       | —       | Comma-separated client ids for `/auth/google` |
| `APPLE_CLIENT_IDS`         | no       | —       | Comma-separated client ids for `/auth/apple`  |
| `MERCADOPAGO_ACCESS_TOKEN` | no       | —       | Mercado Pago API token; also selects sandbox vs production |

## Authentication

`POST /auth/register` and `POST /auth/login` exchange email and password for
a JWT. `POST /auth/google` and `POST /auth/apple` do the same for a social
identity token: the mobile SDK completes the OAuth flow, the app forwards the
resulting `{"id_token": "..."}`, and the API verifies it against the
provider's published keys before returning the **same** `{token, user}`
envelope. There is no OAuth redirect flow in the API.

A social account is matched on the provider's user id, or — when the provider
reports a *verified* email — on that address, which is what links a social
login to an account originally registered with a password.

A provider with no client id configured keeps its route but answers `503`:
without a client id there is no audience to check, and a verifier that accepts
any audience accepts tokens minted for someone else's app.

## Tests and checks

```bash
go build ./...
go vet ./...
gofmt -l .        # must print nothing
go test ./...
```

The test suite has no external dependencies — nothing here needs a live
database.

## Layout

```
cmd/api/            entry point: config load, DB connect, serve, graceful shutdown
internal/config/    environment parsing and validation
internal/db/        pgx connection pool
internal/router/    route table and middleware chain
internal/middleware/ cross-cutting HTTP middleware (request logging)
internal/handler/   shared JSON response helpers
  health/           GET /health liveness endpoint
  auth/             registration / login / social sign-in / me
internal/social/    Google and Apple ID token verification (JWKS)
internal/mercadopago/ Mercado Pago REST client (customer create, card pre-auth)
internal/model/     shared domain types and the social account upsert
migrations/         forward-only SQL, applied by hand
```

Database access is raw SQL through `pgx` — no ORM. Queries stay explicit and
reviewable.
