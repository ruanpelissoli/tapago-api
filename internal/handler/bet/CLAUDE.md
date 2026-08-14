# internal/handler/bet

## Purpose

Two routes, both inside `RequireAuth`, neither reading an `Authorization`
header itself:

- `POST /v1/bets` — puts a user's money at stake. Validates the request,
  reserves the single in-flight bet slot, places a Mercado Pago
  pre-authorisation hold on a saved card, and settles the bet row on the
  result. First caller of `mercadopago.CreatePreAuth`.
- `GET /v1/bets/active` — read-only, one SELECT, no provider call, no writes.
  Returns the caller's in-flight bet so the app can render state on load.

## Key decisions

- **No DB transaction around the Mercado Pago call.** The shape is insert
  `pending` → commit → call MP → update. A transaction is wrong here twice
  over: `PreAuthRequest.ExternalReference` must be the *committed* bet id
  (it seeds the idempotency key), and a rollback after MP succeeded would
  erase our only record of a hold that exists on a real card. Do not
  "improve" this into a transaction.
- **Depends on a two-method `DB` interface** (`QueryRow` + `Exec`), not
  `*pgxpool.Pool`, so the tests run without Postgres — the `handler/payment`
  pattern.
- **Money never touches `float64`.** `stake_amount_brl` decodes as
  `json.Number`, parses through `mercadopago.ParseBRL` (which rejects >2
  decimals), and is stored as `Centavos.String()` into `numeric(12,2)`. The
  hold and the stored stake are therefore the same number by construction.
  The response field is a **string** (`"50.00"`) for the same reason.
- **`payment_method_id` is *our* `payment_methods.id`**, not Mercado Pago's
  `payment_method_id`, which is the card *brand*. The collision is real; the
  brand sent to MP is read from that row's `card_brand`.
- **`betResponse` has no `mp_preauth_id` field at all** — stronger than
  `json:"-"`, nothing to un-hide. Provider-internal, same call as
  `mp_customer_id` being absent from `paymentMethodResponse`.
- **A nil `MercadoPagoClient` keeps both routes mounted.** `Create` answers
  503, checked *before* any insert so no row is written for a hold that was
  never attempted. **`Active` has no such guard on purpose** — it touches no
  provider and must answer 200/404 regardless. Do not add one for symmetry.
- **`Active` returns a bare `betResponse`, not `{"bet": …}`.** The
  `{"payment_methods": […]}` wrapper in `handler/payment` is for a
  *collection* that may need pagination; the in-flight bet is a single
  resource that cannot become a list. Reusing `betResponse` unchanged is also
  what guarantees no `mp_preauth_id`.
- `authenticatedUserID` / `isUUID` / `decodeJSON` are duplicated from
  `handler/payment` rather than exported from it — same trade as that package
  made against `handler/auth`.

## Business logic

Outcome of `CreatePreAuth` decides both the stored status and the response:

| Result | Bet status | HTTP |
|---|---|---|
| success | `active`, `mp_preauth_id` set | 201 + bet |
| `ErrCardDeclined` | `cancelled` | 402 |
| `ErrUnavailable` / `ErrRateLimited` | **stays `pending`** | 503 |
| `ErrAuthentication` / `ErrInvalidRequest` / unknown | `cancelled` | 500 |
| client not configured | no insert | 503 |

- **Why `pending` survives an outage:** the hold's state is genuinely unknown
  and may exist. Cancelling would free the in-flight slot and let the user
  place a **second real hold** on the same card. Blocking them until
  reconciliation is the safe side of that trade. `APIError.PaymentID` is
  logged when present — it is the only handle on that hold.
- **`GET /v1/bets/active` = `status IN ('pending','active')`** — exactly the
  set `bets_user_id_in_flight_key` covers, so at most one row matches and
  `LIMIT 1` is belt-and-braces. `pending` is in the set **deliberately**: a
  bet stranded there by an MP outage still blocks new bets, so the app must
  be able to render "payment pending". All-`completed`/`cancelled` → 404, not
  the last finished bet. `WHERE user_id = $1` *is* the authorisation check;
  no id in the path means nothing to leak.
- **One bet at a time is enforced by `bets_user_id_in_flight_key`**, the
  partial unique index. The `SELECT 1 … status IN ('pending','active')`
  pre-check is a fast-fail optimisation only: two concurrent requests both
  pass it, and the loser's `23505` is what makes the second a 409.
- The card lookup is `WHERE id = $1 AND user_id = $2`; `pgx.ErrNoRows` → 404
  without distinguishing "not yours" from "does not exist", which would make
  the endpoint an oracle for other users' card ids. It joins `users.email`
  because MP requires a payer email.
- Validation (400) runs before any query or provider call: `goal_type` ∈
  `model.GoalType*`, `1 ≤ target_days ≤ 365`, `0 < stake ≤ R$ 10.000,00` with
  ≤2 decimals, `payment_method_id` a canonical UUID.
- **`maxTargetDays` (365) and `maxStakeBRL` (R$ 10.000,00) are assumed
  caps** — nothing in the schema or the brief fixes them. They are named
  constants so changing either is a one-line edit.

## Dependencies

`internal/handler` (response helpers), `internal/middleware` (user id off the
context), `internal/mercadopago` (client interface, sentinels, `Centavos`,
`ParseBRL`), `internal/model` (`GoalType`, `BetStatus`), pgx/pgconn. Mounted
by `internal/router`; the table comes from `migrations/003_create_bets.sql`,
applied by hand.

## Gotchas

- **Reconciling a bet left `pending` by an MP outage is not implemented.**
  Until it is, that user cannot place a new bet. This is the top follow-up
  for this package.
- `bets.goal_type` has **no CHECK** yet, so this handler is the only gate
  between a typo and a goal nothing can resolve.
- A comma decimal (`"50,00"`) is rejected at decode time even though
  `ParseBRL` accepts one: `json.Number` requires valid JSON number syntax,
  quoted or not. The wire format is a dot.
- Never log the request body or the decode error — the same rule as
  `handler/payment`. Provider detail (`status_detail`, MP's message) never
  reaches a response body; the sentinel picks the status, the detail goes to
  `slog`.
- If the settling `UPDATE` fails after MP succeeded, the response is a 500 and
  the row is recoverable **only** from the log line carrying the bet id and
  the pre-auth id. Do not weaken that log.
- Reading the *active* bet exists; **bet history/listing, cancellation,
  capturing/releasing the hold, webhooks, and any notion of progress
  (days elapsed, end date) are all unimplemented** — a hold placed here is
  never released.
