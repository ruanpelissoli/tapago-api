# migrations

## Purpose

Numbered, forward-only SQL files describing the database schema. There is no
migration runner yet — each file is applied by hand:

```bash
psql "$DATABASE_URL" -f migrations/00N_name.sql
```

## Key decisions

- **Every statement is idempotent** (`IF NOT EXISTS`, `ADD COLUMN IF NOT
  EXISTS`). Without a runner tracking which files have been applied, re-running
  one must be harmless.
- **No down migrations.** Rolling a schema backwards in production loses data;
  the fix for a bad migration is another migration.
- **No ORM, no generated schema.** These files are the source of truth and the
  queries in `internal/` are written against them by hand.

## Business logic

- `001_create_users.sql` — the `users` table. `email` is `UNIQUE`, and
  handlers normalise to lower case before writing, which is what makes that
  index behave case-insensitively without the `citext` extension.
- `002_add_social_ids.sql` — nullable `google_id` / `apple_id` with **partial
  unique indexes**, so one provider account cannot log in as two users while
  the many rows with no link stay valid. It also drops `NOT NULL` from
  `password_hash`: an account created purely through social sign-in has no
  password, and a sentinel `''` would invite code that treats it as
  verifiable. Callers must handle NULL — see `internal/handler/auth`.
- `003_create_bets.sql` — the `bets` table. A **partial unique index** on
  `bets(user_id) WHERE status IN ('pending','active')` is the one-active-bet
  rule: it lives in the database because two concurrent inserts would both
  pass an application-level "does this user already have a bet?" check, and
  only a unique index makes the second one fail. Finished bets leave the
  index, so a user can start a new bet immediately and keep unlimited
  history. `status` is `text` + `CHECK` rather than an `ENUM` (a CHECK is
  editable; adding an enum value is a type migration), and `goal_type` has no
  CHECK yet because the taxonomy is undecided.
- `004_create_payment_methods.sql` — saved Mercado Pago cards. A partial
  unique index on `payment_methods(user_id) WHERE is_default` stops a user
  ending up with two default cards, which would make "charge the default"
  ambiguous; zero cards, or cards with no default, stay valid. `last_four`
  is CHECKed to exactly four digits — no PAN, CVV or expiry may ever be
  stored here, only provider identifiers and display metadata.

## Dependencies

`pgcrypto` (for `gen_random_uuid()` on PostgreSQL < 13; a no-op from 13 on).
`internal/model` and `internal/handler/auth` write the queries that depend on
these columns; changing a column name breaks them at query time, not at
compile time.

## Gotchas

- **Applying a file is a manual step.** Code merged before its migration runs
  fails with an "column does not exist" error at request time, not at startup.
- **Except under `docker compose`**, where this directory is mounted at the
  database's `docker-entrypoint-initdb.d`: PostgreSQL runs every `*.sql` here,
  in name order, but **only when the data volume is empty**. So a new
  migration reaches a colleague's existing local database only if they apply
  it by hand or run `docker compose down -v`. The numbering is what makes the
  order correct — do not rename a file out of sequence. Non-`.sql` files here
  (this one) are ignored by that mechanism.
- `ON CONFLICT (email)` in `model.UpsertSocialUser` depends on the unique
  constraint from 001. Dropping it silently turns the upsert into a duplicate
  insert.
- Adding a `NOT NULL` column to `users` without a default will break the
  social upsert, which only supplies email, name and one provider id.
- `bets.user_id` is `ON DELETE RESTRICT`, so deleting a user who has ever
  placed a bet now fails with a foreign-key violation. That is deliberate —
  bets are financial records — but any account-deletion path must resolve the
  bets first. `payment_methods.user_id` cascades instead.
