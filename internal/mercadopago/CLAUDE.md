# internal/mercadopago

## Purpose

Wraps the two Mercado Pago REST calls this API makes server-side: create a
customer, and place a pre-authorization hold on a card (`capture=false`).
Pure integration plumbing — no handlers, no routes, no database. It produces
the values `payment_methods.mp_customer_id` and `bets.mp_preauth_id` hold.

## Key decisions

- **The server never sees a card number.** `POST /v1/card_tokens` takes a raw
  PAN and CVV; calling it here would put this service in PCI scope, the exact
  thing the comment atop `004_create_payment_methods.sql` rules out. The
  mobile SDK tokenises on-device, so `CardToken` is the only card-shaped value
  in this package.
- **No Mercado Pago SDK.** Two JSON POSTs over `net/http`, as in
  `internal/social`. The hard part is the error mapping, and that has to be
  ours regardless.
- **One base URL for sandbox and production** — the access token selects the
  environment, so there is no environment flag to fall out of step with the
  credential. `WithBaseURL` is **test-only**: the token rides on every request
  as a bearer credential, so repointing the client hands a live payment
  credential to whoever runs the other host.
- **`New` refuses a blank token** (`ErrNoAccessToken`), turning a startup
  misconfiguration into a startup failure rather than a runtime payment one.
- **Money is `Centavos` (int64), never `float64`** — required by
  `internal/model/CLAUDE.md`, and this path is why: R$ 19,99 is not exactly
  representable in binary, and the hold must equal `bets.stake_amount_brl` to
  the centavo. Conversion to a JSON number happens only in
  `Centavos.MarshalJSON`, always with two decimals.
- **No retry inside the client.** One attempt, caller's context honoured.
  Retry policy belongs to the caller, the only layer that knows whether the
  user is still waiting. `APIError.Retryable()` says whether one could help.

## Business logic

- **A declined card is HTTP 201.** `POST /v1/payments` answers `201 Created`
  with `status:"rejected"` and a `cc_rejected_*` `status_detail`. Checking
  only the HTTP code records a hold that does not exist, so `CreatePreAuth`
  branches on the body's `status`.
- Only `authorized` is success (`status_detail: "pending_capture"`).
  `rejected`/`cancelled` are `ErrCardDeclined`. Everything else — including
  `in_process`, `pending`, `approved` — is `ErrUnavailable`: the hold is
  unconfirmed, but the issuer has not refused, so calling it a decline would
  be a lie to the user.
- **`ErrUnavailable` must never be reported as a decline.** The
  `internal/social` `ErrKeysUnavailable` lesson: during an MP outage, telling
  users their card failed sends them to their bank over our problem.
- Body: `capture:false`, `installments:1`, `binary_mode:true`,
  `transaction_amount`, `token`, `payment_method_id`, `description`,
  `external_reference`, `payer`. `payer.type` is `"customer"` **only** when
  there is a customer id — MP rejects an id without a type (error 4013), and
  the guest flow is email-only.
- `PreAuthRequest` is validated locally first, so a request we know is bad
  never consumes the single-use card token.
- **`X-Idempotency-Key` is SHA-256 of `"<namespace>:<value>"`** — the bet id
  for a pre-auth, the normalised email for a customer. Never random: a random
  key makes a retry look like a new request and places a **second hold on a
  real person's balance**. Hashed so the header stays bounded ASCII and our
  ids stay out of a third party's logs.
- Emails are trimmed and lower-cased, matching auth, so one user cannot end up
  with two customers differing only in case.
- `ErrCustomerAlreadyExists` (MP cause code **101** on a 400) is a normal
  outcome, not a failure. Both the code and the message text are checked, as
  the cause array is not always present.

## Dependencies

Standard library only. Imported by nothing yet; the bet flow wires it in later
through the `MercadoPagoClient` interface, so handlers can use a fake. The
token comes from `config.MercadoPagoAccessToken`, which is optional — no
token, no client, so payment routes answer 503.

## Gotchas

- **`capture` has no `omitempty`.** Adding one drops `false` from the JSON and
  MP captures the money instead of holding it.
- **No error here may carry the access token or request URL.** `APIError`
  holds only MP's own status/detail/message.
- Payment ids are JSON **numbers** (int64, over int32 in production), customer
  ids **strings** (`<sellerId>-<hash>`). `flexibleID` decodes both via
  `json.Number`, so no float64 touches an id; a plain `string` field fails
  silently on the number form and turns every success into "carried no id".
- **MP has two error envelopes**: `{message,error,status,cause[]}` from the
  service layer, and a bare `{code,message}` from the edge gateway (what a bad
  token actually hits), which can also serve HTML on a 5xx. `cause[].code` is
  a number on some endpoints, a string on others. `parseErrorBody` tolerates
  all of it and never indexes `cause[0]`; the sentinel comes from the HTTP
  status alone, so this parsing is not load bearing.
- **MP does not return the existing customer id on a 101** — no such field
  exists. `APIError.CustomerID` is best-effort prose scraping and is normally
  empty; callers need their own fallback (`payment_methods.mp_customer_id`, or
  `GET /v1/customers/search?email=`, not implemented here).
- `X-Idempotency-Key` is documented only on `/v1/payments`. We send it on
  `/v1/customers` too (harmless), but do not rely on it to dedupe customers.
- `binary_mode:true` is sent per the task spec though MP's own reserve example
  uses `false`. It should be a no-op for a reserve; if MP ever rejects the
  combination, look here first.
- A reserve **expires after ~5 days** and MP auto-cancels it. Nothing tracks
  that. Capture (`PUT /v1/payments/{id}` `capture:true`), cancel, webhooks,
  saving cards to a customer and refunds are all unimplemented — a hold placed
  here is never released by this package.
- Bodies are capped at 1 MiB by `io.LimitReader`; a larger one truncates and
  then fails to parse as `ErrUnavailable`.
- Sandbox credentials require payer emails matching
  `test_payer_[0-9]{1,10}@testuser.com` (error 128 otherwise).
