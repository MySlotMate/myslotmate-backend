-- +migrate Up
-- Private (passkey-gated) events + comp coupons.
--
-- Private events stay LISTED in discovery (a lock badge is shown); the passkey is
-- required only at the Book step, not to view. passkey_grants_free lets the same
-- passkey ALSO comp a paid booking to zero — the host comps the guest, so the
-- booking rides the existing totalAmount == 0 path (no wallet debit, no ledger,
-- no fee split).
ALTER TABLE events ADD COLUMN IF NOT EXISTS is_private BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE events ADD COLUMN IF NOT EXISTS access_passkey TEXT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS passkey_grants_free BOOLEAN NOT NULL DEFAULT FALSE;

-- Coupons: a comp code that waives a booking to free (no partial discounts in v1).
-- Scope is one event (event_id set) or all of a host's events (event_id NULL).
CREATE TABLE IF NOT EXISTS coupons (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id         UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    event_id        UUID REFERENCES events(id) ON DELETE CASCADE,
    code            TEXT NOT NULL,
    max_redemptions INT,                       -- NULL = unlimited total uses
    times_redeemed  INT NOT NULL DEFAULT 0,
    per_user_limit  INT,                       -- NULL = unlimited uses per guest
    valid_from      TIMESTAMPTZ,
    valid_until     TIMESTAMPTZ,
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Codes match case-insensitively and are unique per host.
CREATE UNIQUE INDEX IF NOT EXISTS coupons_host_code_key ON coupons (host_id, lower(code));
CREATE INDEX IF NOT EXISTS idx_coupons_event ON coupons (event_id);

-- Which comp a booking used (NULL = none). Drives per-user redemption counts.
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS coupon_id UUID REFERENCES coupons(id);
CREATE INDEX IF NOT EXISTS idx_bookings_coupon ON bookings (coupon_id);

-- +migrate Down
ALTER TABLE bookings DROP COLUMN IF EXISTS coupon_id;
DROP TABLE IF EXISTS coupons;
ALTER TABLE events DROP COLUMN IF EXISTS passkey_grants_free;
ALTER TABLE events DROP COLUMN IF EXISTS access_passkey;
ALTER TABLE events DROP COLUMN IF EXISTS is_private;
