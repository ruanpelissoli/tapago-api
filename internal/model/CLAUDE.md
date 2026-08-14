# internal/model

## Purpose

Shared domain types (users, accounts, transactions, ...) plus the small
amount of storage logic that would otherwise be duplicated across handlers —
today that is `UpsertSocialUser`, the account resolution shared by
`/auth/google` and `/auth/apple`.

## Key decisions

- **Domain types, not wire types.** Request/response DTOs stay next to their
  handlers. Reusing one struct for both couples the public API to the
  database schema: adding a column would silently change API output, and a
  field only needed for JSON would pollute the domain.
- No ORM tags or framework annotations — scanning is explicit at the query
  site.
- **`UpsertSocialUser` lives here rather than in `handler/auth`.** Both
  provider endpoints need identical create-or-link semantics, and two copies
  of that SQL would drift; the divergence would be a security bug, not a
  cosmetic one. The package takes a one-method `Querier` interface, so it
  still does not depend on `internal/db` and is testable without Postgres.
  This is a deliberate relaxation of the older "no behaviour that needs a
  database" rule, scoped to logic that is genuinely shared.
- **Provider names are duplicated as `ProviderGoogle`/`ProviderApple`** rather
  than imported from `internal/social`, keeping the dependency direction
  one-way. The values must stay in sync with `social.Provider*`.
- **`Bet.StakeAmountBRL` is `pgtype.Numeric`.** Not `float64` (BRL settles in
  centavos; binary floats would lose or invent them before a real charge) and
  not a third-party decimal package (`pgtype` ships inside the already-required
  `github.com/jackc/pgx/v5`, so it costs no new module and scans
  `numeric(12,2)` exactly). The price is ergonomic — arithmetic goes through
  `Int`/`Exp` or `Value`/`Scan`, not an operator — and is accepted on purpose.
- **`Bet` and `PaymentMethod` are pure data.** No constructors, no `IsActive()`
  helper, no re-statement of the DB CHECKs (`target_days > 0`, `stake > 0`,
  the last-four regex). The one-active-bet rule lives in the partial unique
  index `bets_user_id_in_flight_key`; duplicating any of it in Go just creates
  a second place that can disagree. Behaviour arrives with its handlers.

## Business logic

`UpsertSocialUser` resolves in a fixed order, and the order is the design:

1. **Match on the provider subject** (`google_id` / `apple_id`). This is every
   sign-in after the first, and it must not consult the email — a user can
   change their address at the provider without losing their account.
2. **Otherwise insert, `ON CONFLICT (email) DO UPDATE`.** This is what links
   a social login to an account that was registered with a password.

- The conflict arm uses `COALESCE(users.<col>, EXCLUDED.<col>)`, so an
  existing link is never overwritten; the caller compares the returned id with
  the one it asked for and gets `ErrProviderConflict` on a mismatch. Without
  that, a second Google account claiming the same address would take over the
  first one's user.
- The name is only filled in when the row has none — a display name the user
  chose here must survive whatever the provider currently holds.
- `Email` must already be **verified** by the caller. Matching an existing
  account on an unverified address hands it to whoever controls the provider
  profile. No email and no existing link → `ErrEmailRequired`.
- Empty name falls back to the email local part; Apple sends no name at all.
- A unique violation on the provider index means a concurrent first sign-in
  won the race, so the row is re-read instead of reported as a conflict.
- `User.PasswordHash` is `*string`: nil means a social-only account with no
  password, which the login path must treat as unauthenticable rather than
  feeding to a verifier.

## Dependencies

pgx (row/error types) and the standard library. Must not import
`internal/db`, `internal/handler`, or `internal/router`.

## Gotchas

- **The provider column is interpolated into the SQL.** It is safe only
  because it comes from the fixed `socialColumns` map and an unknown provider
  is refused before any statement is built. Never extend that pattern to a
  value that can come from a request; user data goes through placeholders.
- `ON CONFLICT (email)` needs the unique index from `001_create_users.sql`;
  the provider columns and their partial unique indexes come from
  `002_add_social_ids.sql`. Running the code against an un-migrated database
  fails at query time, not at startup.
- Money is `numeric` in Postgres; never model it as `float64`. Timestamps are
  `timestamptz` and UTC everywhere.
- **`BetStatus` values must match the CHECK in `003_create_bets.sql`
  character-for-character.** A mismatch compiles and fails at query time, on a
  user's request. `GoalType` is the inverse trap: `goal_type` has *no* CHECK
  yet (the taxonomy is unsettled), so the constants are advisory and the
  database accepts any text — when the CHECK lands, write it from that list.
- `PaymentMethod.LastFour` is a `string`, not an `int`: `"0042"` is valid and
  would lose its leading zeros as a number. Nothing in that struct may ever
  hold a PAN, CVV or expiry — provider ids and display metadata only, which is
  what keeps this database out of PCI scope.
- Never give a domain struct a `json:"-"` password field and pass it to a
  response helper — build an explicit DTO instead.
