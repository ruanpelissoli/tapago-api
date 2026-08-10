-- 001_create_users.sql
--
-- Users table backing email/password authentication.
--
-- Run manually for this milestone; there is no migration runner yet:
--   psql "$DATABASE_URL" -f migrations/001_create_users.sql

-- gen_random_uuid() lives in pgcrypto on PostgreSQL < 13; it is built in
-- from 13 onward. Creating the extension is a no-op on newer servers and
-- keeps the migration portable.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- citext would make this case-insensitive at the type level, but it is
    -- an extension; handlers normalise to lower case before writing so a
    -- plain unique index is enough and keeps the schema dependency-free.
    email         text        NOT NULL UNIQUE,
    -- bcrypt output is a fixed 60-char string; text avoids a pointless
    -- length constraint if the cost or algorithm prefix ever changes.
    password_hash text        NOT NULL,
    name          text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
