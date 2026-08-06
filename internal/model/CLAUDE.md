# internal/model

## Purpose

Shared domain types (users, accounts, transactions, ...). Empty by design —
the package exists so the layout is settled before the first feature lands.

## Key decisions

- **Domain types, not wire types.** Request/response DTOs stay next to their
  handlers. Reusing one struct for both couples the public API to the
  database schema: adding a column would silently change API output, and a
  field only needed for JSON would pollute the domain.
- No ORM tags or framework annotations — scanning is explicit at the query
  site in the feature package.

## Business logic

None yet. When adding types:

- Money is `numeric` in Postgres; do not model it as `float64`. Use an
  integer minor-unit type or `pgtype.Numeric`.
- Timestamps are `timestamptz` and UTC everywhere.
- Never give a domain struct a `json:"-"` password field and then pass it to
  a response helper — build an explicit DTO instead.

## Dependencies

Standard library (and possibly `pgtype`) only. This package must not import
`internal/db`, `internal/handler`, or `internal/router` — the dependency
direction is one-way, toward the model.

## Gotchas

- Keep it free of behavior that needs a database or an HTTP request; those
  belong in the feature package.
