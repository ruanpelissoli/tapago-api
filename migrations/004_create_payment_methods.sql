-- 004_create_payment_methods.sql
--
-- Saved Mercado Pago cards a user can stake a bet against.
--
-- Run manually for this milestone; there is no migration runner yet:
--   psql "$DATABASE_URL" -f migrations/004_create_payment_methods.sql

-- Nothing in this table may ever hold a full card number (PAN), a CVV or an
-- expiry date. Only Mercado Pago identifiers and the display metadata needed
-- to render "Visa ···· 4242" belong here; the card data itself stays with the
-- provider, which keeps this database out of PCI scope.
CREATE TABLE IF NOT EXISTS payment_methods (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ON DELETE CASCADE, the opposite call from bets: a saved card is
    -- meaningless without its user and carries no financial record of its own.
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Mercado Pago card tokens are short-lived and single-use; the durable
    -- handle for charging is the customer + card pairing, so this column will
    -- likely need revisiting when the payment integration lands. Kept under
    -- this name for now because downstream tasks are written against it.
    mp_card_token  text        NOT NULL,
    mp_customer_id text        NOT NULL,
    -- Exactly four digits, never a PAN — the CHECK is a guard rail against a
    -- caller ever writing the full number into a display-only column.
    last_four      text        NOT NULL CHECK (last_four ~ '^[0-9]{4}$'),
    card_brand     text        NOT NULL,
    is_default     boolean     NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- A user cannot have two default cards. Only rows with is_default = true are
-- indexed, so zero cards, or several cards with none marked default, remain
-- valid — the rule forbids a second default, not the absence of one.
CREATE UNIQUE INDEX IF NOT EXISTS payment_methods_user_id_default_key
    ON payment_methods (user_id) WHERE is_default;

-- Listing a user's saved cards.
CREATE INDEX IF NOT EXISTS payment_methods_user_id_idx
    ON payment_methods (user_id);
