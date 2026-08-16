# tapago-api

HTTP API for tapago, written in Go with the standard library plus
[chi](https://github.com/go-chi/chi) for routing and
[pgx](https://github.com/jackc/pgx) for PostgreSQL.

## Requirements

- Docker with Compose v2 — or, to run the binary directly, Go 1.25 or newer
  plus a reachable PostgreSQL instance

## Running locally with Docker

```bash
cp .env.example .env      # then set JWT_SECRET
docker compose up --build
```

That starts two services: `db` (PostgreSQL 17) and `api`. The API waits for
the database to report healthy before it starts, because it exits rather than
boot without one.

`migrations/` is mounted into the database's `docker-entrypoint-initdb.d`, so
on a **fresh** volume PostgreSQL applies `001…004` in numbered order — the
same thing the manual `psql -f` step does. It does not run them again against
an existing volume: after adding a migration, either apply it by hand or
recreate the database with `docker compose down -v`, which deletes all local
data.

Useful commands:

```bash
docker compose ps                 # service status and health
docker compose logs -f api        # follow API logs
docker compose down               # stop, keep the data volume
docker compose down -v            # stop and delete the data volume
```

`.env` is git-ignored and is read by compose automatically. Compose builds
`DATABASE_URL` itself, pointing at the `db` service — the value in `.env` is
for running the binary on the host, where the database is on `localhost`.
`API_PORT` and `POSTGRES_PORT` change the published host ports only.

## Running locally without Docker

```bash
cp .env.example .env      # then edit DATABASE_URL / JWT_SECRET
set -a && . ./.env && set +a
go run ./cmd/api
```

`docker compose up db` starts just the database if you want the API on the
host and PostgreSQL in a container.

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

Compose-only extras — they configure the local stack, not the API process:
`API_PORT`, `POSTGRES_PORT`, `POSTGRES_USER`, `POSTGRES_PASSWORD`,
`POSTGRES_DB`. The `POSTGRES_*` values are read once, when the data volume is
first created; changing them later needs `docker compose down -v`.

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

## Payment methods

Both routes require a bearer token.

| Route                      | Body                                       | Returns |
| -------------------------- | ------------------------------------------ | ------- |
| `POST /v1/payment-methods` | `{card_token, last_four, card_brand}`      | `201`   |
| `GET /v1/payment-methods`  | —                                          | `200`   |

The mobile SDK tokenises the card on the device; the API never sees a card
number. Saving the first card creates a Mercado Pago customer for the user and
marks the card as their default; later cards reuse that customer and are not
default. `GET` returns `{"payment_methods": [...]}`, ordered default first then
newest first, and `[]` — never `null` — when there are none.

A response **never** contains `mp_card_token` or `mp_customer_id`: the token is
a live credential and the customer id is provider internal. Each method is
returned as `{id, last_four, card_brand, is_default, created_at}`.

With no `MERCADOPAGO_ACCESS_TOKEN` configured the routes stay mounted and
answer `503` rather than disappearing, so a missing token looks like a missing
token and not like a wrong path. Mercado Pago being unreachable or rate
limiting us is also `503` — never reported as a declined card.

The table comes from `migrations/004_create_payment_methods.sql`, which must
be applied by hand.

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
  payment/          saved Mercado Pago cards (create / list)
internal/social/    Google and Apple ID token verification (JWKS)
internal/mercadopago/ Mercado Pago REST client (customer create, card pre-auth)
internal/model/     shared domain types and the social account upsert
migrations/         forward-only SQL, applied by hand
```

Database access is raw SQL through `pgx` — no ORM. Queries stay explicit and
reviewable.
