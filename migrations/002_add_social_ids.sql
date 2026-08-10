-- 002_add_social_ids.sql
--
-- Social sign-in: link a users row to a Google and/or Apple account.
--
-- Run manually for this milestone; there is no migration runner yet:
--   psql "$DATABASE_URL" -f migrations/002_add_social_ids.sql

-- The provider subject ("sub" claim). Nullable because most accounts are
-- email/password only, and one account may be linked to both providers.
--
-- Apple's sub is scoped to the app's team, Google's is globally stable; both
-- are opaque strings, so text rather than a fixed-width type.
ALTER TABLE users ADD COLUMN IF NOT EXISTS google_id text;
ALTER TABLE users ADD COLUMN IF NOT EXISTS apple_id  text;

-- A provider account must not be usable to log in as two different users.
-- A partial unique index (rather than a UNIQUE constraint) is what allows
-- many rows to keep a NULL provider id: in Postgres NULLs are distinct under
-- a plain unique index too, but stating it explicitly documents the intent
-- and keeps the index small.
CREATE UNIQUE INDEX IF NOT EXISTS users_google_id_key
    ON users (google_id) WHERE google_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS users_apple_id_key
    ON users (apple_id) WHERE apple_id IS NOT NULL;

-- An account created purely through social sign-in has no password, and a
-- sentinel value ('' or a hash of a random string) would be worse than NULL:
-- it invites code that treats it as verifiable. NULL forces the login path
-- to make an explicit decision, which it now does (no password set -> the
-- same opaque "invalid credentials" as a wrong password).
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
