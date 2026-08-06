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

| Variable       | Required | Default | Purpose                                |
| -------------- | -------- | ------- | -------------------------------------- |
| `PORT`         | no       | `8080`  | HTTP listen port                       |
| `DATABASE_URL` | yes      | —       | PostgreSQL connection string           |
| `JWT_SECRET`   | no\*     | —       | Signs access tokens (\*required by auth) |

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
  auth/             registration / login / refresh (stub)
internal/model/     shared domain types (stub)
```

Database access is raw SQL through `pgx` — no ORM. Queries stay explicit and
reviewable.
