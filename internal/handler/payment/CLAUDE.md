# internal/handler/payment

## Purpose

Saved Mercado Pago cards: `POST /v1/payment-methods` stores a card token as a
payment method, `GET /v1/payment-methods` lists the caller's own. Both sit
inside `RequireAuth`; this package never reads an `Authorization` header
itself. It is the first caller of `internal/mercadopago`.

## Key decisions

- **Depends on a two-method `DB` interface, not `*pgxpool.Pool`** — `QueryRow`
  for create, `Query` for list. That is what keeps the tests free of Postgres,
  as in `handler/auth`.
- **`paymentMethodResponse` has no `mp_card_token` and no `mp_customer_id`
  field at all**, which is stronger than `json:"-"`: there is nothing to
  un-hide. The token is a live credential and the customer id is provider
  internal. Same argument as `userResponse` having no password field. A test
  scans the raw response bytes for both, so a future field cannot slip in.
- **The list is wrapped in `{"payment_methods": […]}`** rather than a
  top-level array, leaving room for pagination without a breaking change. The
  slice is pre-allocated so an empty result is `[]`, never `null`.
- **`is_default` is computed in SQL** (`NOT EXISTS (SELECT 1 …)` inside the
  INSERT). A SELECT-then-INSERT would leave a read-then-write window; the
  partial unique index `payment_methods_user_id_default_key` is the source of
  truth, and losing that race (SQLSTATE **23505**) retries the insert **once**
  as non-default. No pre-check — the same reasoning as duplicate email in
  `handler/auth`.
- **A nil `MercadoPagoClient` keeps both routes mounted and answers 503.** A
  route that appears and vanishes with the environment is far harder to debug
  than one returning a clear server error.
- **`decodeJSON`/`isUUID` are duplicated from `handler/auth`** rather than
  exported from it: that package documents itself as handling credentials
  only, and widening its API for ~25 lines of syntax gating is the worse
  trade.

## Business logic

- Create returns **201**, list **200**. Validation (400) runs before any
  query or provider call.
- **Customer resolution:** if the user already has any card, its stored
  `mp_customer_id` is reused and **no** Mercado Pago call is made. Otherwise
  the user's email is read (`pgx.ErrNoRows` → **401**, a token for a deleted
  user, the `auth.Me` precedent) and `CreateCustomer` runs.
- `ErrCustomerAlreadyExists` is a normal outcome, but only recoverable when
  MP disclosed the id in `APIError.CustomerID`. It usually does not — there is
  no such field on a cause-101 400 — and with no stored id either, that is a
  **500**. Recovering it needs `GET /v1/customers/search`, which
  `internal/mercadopago` does not implement; that is a follow-up task.
- `ErrUnavailable` / `ErrRateLimited` → **503** `payment provider
  unavailable`, **never** presented as a card problem. `ErrAuthentication` /
  `ErrInvalidRequest` / anything unrecognised → **500**, logged at error.
- `last_four` must be exactly four ASCII digits, mirroring the CHECK in
  migration 004 so a bad value is a 400 rather than a 500 out of the
  constraint. It stays a **string**: `"0042"` loses its zeros as a number.
- `card_brand` is lower-cased before storage (MP uses `visa`, `master`,
  `elo`); `card_token` ≤ 512 bytes, `card_brand` ≤ 40.
- The request body does **not** accept `is_default`. Changing which card is
  default is a separate endpoint and a follow-up task.

## Dependencies

`internal/handler` (response helpers), `internal/middleware` (reads the user
id off the context), `internal/mercadopago` (client interface + sentinels),
pgx/pgconn. Mounted by `internal/router`; the table comes from
`migrations/004_create_payment_methods.sql`, applied by hand.

## Gotchas

- **Never log the request body or the decode error** — both quote a live card
  token. `decodeJSON` records only that decoding failed.
- Provider detail never reaches a response body; the sentinel picks the
  status, the detail goes to `slog`.
- **`/v1` is local to these two routes.** The rest of the surface
  (`/health`, `/auth/*`, `/me`) is unversioned; whether to move it is a
  separate decision.
- `mp_card_token` is stored because the migration requires it, though MP
  tokens are short-lived and single-use. Revisiting that column belongs to
  the payment-integration task.
- The 23505 retry is the only thing keeping a concurrent first-card race from
  a 500. Do not "simplify" it into a pre-flight SELECT.
