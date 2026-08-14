-- 003_create_bets.sql
--
-- Bets table: a user's staked commitment to a habit goal.
--
-- Run manually for this milestone; there is no migration runner yet:
--   psql "$DATABASE_URL" -f migrations/003_create_bets.sql

CREATE TABLE IF NOT EXISTS bets (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ON DELETE RESTRICT, not CASCADE: a bet is a financial record tied to a
    -- real pre-authorisation. Removing a user who still has bets must be a
    -- deliberate, explicit operation rather than a silent side effect.
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    -- Deliberately unconstrained: the goal taxonomy is not settled yet. Add a
    -- CHECK here (same shape as status below) once the allowed goal types are
    -- defined, so a typo cannot create a goal nothing can resolve.
    goal_type        text        NOT NULL,
    target_days      int         NOT NULL CHECK (target_days > 0),
    -- Money is numeric, never a float: BRL settles in centavos and binary
    -- floating point cannot represent them exactly. Scale 2 is the currency's.
    stake_amount_brl numeric(12,2) NOT NULL CHECK (stake_amount_brl > 0),
    -- A CHECK rather than a Postgres ENUM: adding a value to an enum is a type
    -- migration and enum ordering leaks into comparisons, whereas a CHECK is
    -- edited by dropping and re-adding one constraint. It also keeps the
    -- schema dependency-free, matching 001/002.
    status           text        NOT NULL CHECK (status IN ('pending', 'active', 'completed', 'cancelled')),
    -- Nullable: the bet row exists before Mercado Pago has pre-authorised the
    -- stake, and never gets one if the bet is cancelled first.
    mp_preauth_id    text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- The one-active-bet business rule, enforced by the database rather than by
-- application code alone: two concurrent inserts of an in-flight bet for the
-- same user cannot both succeed — the second fails with a unique violation.
-- Only 'pending' and 'active' rows are indexed, so finishing a bet
-- ('completed'/'cancelled') drops it out of the index and frees the user to
-- start a new one immediately, while unlimited history stays valid.
CREATE UNIQUE INDEX IF NOT EXISTS bets_user_id_in_flight_key
    ON bets (user_id) WHERE status IN ('pending', 'active');

-- The partial index above covers at most one row per user, so it cannot serve
-- "list my past bets". This is the index that does; a plain non-unique index
-- on user_id alone would be redundant with it.
CREATE INDEX IF NOT EXISTS bets_user_id_created_at_idx
    ON bets (user_id, created_at DESC);

-- Same pattern as the provider indexes in 002: one Mercado Pago
-- pre-authorisation must never be attached to two bets, while the many rows
-- with no pre-authorisation yet stay valid.
CREATE UNIQUE INDEX IF NOT EXISTS bets_mp_preauth_id_key
    ON bets (mp_preauth_id) WHERE mp_preauth_id IS NOT NULL;
