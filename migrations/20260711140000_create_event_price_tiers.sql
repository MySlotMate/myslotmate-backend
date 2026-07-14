-- +migrate Up
-- Named ticket tiers per event (e.g. General / VIP / Student). Opt-in: events
-- with no tiers keep using events.price_cents as before. A booking picks ONE
-- tier and snapshots its unit price onto the booking row so that later tier
-- edits never alter money already moved (refunds/ledger read the booking).

CREATE TABLE IF NOT EXISTS event_price_tiers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    price_cents BIGINT NOT NULL CHECK (price_cents >= 0),
    capacity    INT,                       -- reserved; NOT enforced in v1
    sort_order  INT NOT NULL DEFAULT 0,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_event_price_tiers_event ON event_price_tiers (event_id);

-- Which tier a booking used + the per-ticket price snapshot at booking time.
-- Both nullable: legacy / free / no-tier bookings leave them NULL.
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS price_tier_id UUID REFERENCES event_price_tiers(id);
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS unit_price_cents BIGINT;

-- +migrate Down
ALTER TABLE bookings DROP COLUMN IF EXISTS unit_price_cents;
ALTER TABLE bookings DROP COLUMN IF EXISTS price_tier_id;
DROP TABLE IF EXISTS event_price_tiers;
