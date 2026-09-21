-- +migrate Up
-- Monthly pass: one up-front payment that covers every session of a recurring
-- event for 30 days. Opt-in per event — an event with monthly_pass_price_cents
-- NULL behaves exactly as before.
--
-- Money moves ONCE, at purchase, on the same wallet-debit + 4-entry ledger
-- split a normal booking uses. Reserving a covered date afterwards writes a
-- booking at amount_cents = 0 with pass_id set: no wallet debit, no ledger, no
-- host earnings — the host was already credited for the whole pass. That is the
-- same "seat with no money" shape as an offline-paid import, and must stay so,
-- or a pass holder's reservations would credit the host twice.

ALTER TABLE events ADD COLUMN IF NOT EXISTS monthly_pass_price_cents   BIGINT CHECK (monthly_pass_price_cents > 0);
ALTER TABLE events ADD COLUMN IF NOT EXISTS monthly_pass_session_limit INT    CHECK (monthly_pass_session_limit > 0);  -- NULL = every session
ALTER TABLE events ADD COLUMN IF NOT EXISTS monthly_pass_capacity      INT    CHECK (monthly_pass_capacity > 0);       -- NULL = unlimited passes

CREATE TABLE IF NOT EXISTS user_passes (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id          UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Snapshot of what was paid. Later edits to the event's pass price must
    -- never change money already moved (same rule as event_price_tiers).
    price_cents       BIGINT NOT NULL CHECK (price_cents > 0),
    service_fee_cents BIGINT NOT NULL DEFAULT 0,
    net_earning_cents BIGINT NOT NULL DEFAULT 0,

    sessions_included INT,                      -- NULL = every session in the window
    sessions_used     INT NOT NULL DEFAULT 0 CHECK (sessions_used >= 0),

    valid_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_until       TIMESTAMPTZ NOT NULL,

    -- active: usable now. expired: window closed. refunded: cancelled and money
    -- returned. Only 'active' rows may reserve a session.
    status            TEXT NOT NULL DEFAULT 'active',

    payment_id        UUID REFERENCES payments(id),
    idempotency_key   TEXT UNIQUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_user_passes_user  ON user_passes (user_id);
CREATE INDEX IF NOT EXISTS idx_user_passes_event ON user_passes (event_id);

-- One live pass per guest per event. Expired passes are flipped to 'expired'
-- at purchase time, so this never blocks buying next month's pass.
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_passes_one_active
    ON user_passes (event_id, user_id) WHERE status = 'active';

-- Which pass covered this booking. NULL = an ordinary paid/free booking.
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS pass_id UUID REFERENCES user_passes(id);
CREATE INDEX IF NOT EXISTS idx_bookings_pass ON bookings (pass_id);

-- +migrate Down
ALTER TABLE bookings DROP COLUMN IF EXISTS pass_id;
DROP TABLE IF EXISTS user_passes;
ALTER TABLE events DROP COLUMN IF EXISTS monthly_pass_capacity;
ALTER TABLE events DROP COLUMN IF EXISTS monthly_pass_session_limit;
ALTER TABLE events DROP COLUMN IF EXISTS monthly_pass_price_cents;
