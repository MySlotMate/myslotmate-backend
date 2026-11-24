-- +migrate Up
-- Event Boost (roadmap task 21): a host asks MySlotMate to promote one event;
-- the admin team proposes a commission per ticket type (12–20%, host guide
-- bands); once the host accepts and an admin goes live, that commission is
-- taken on the event's tickets sold while live. Additive only: the per-host
-- commission (hosts.platform_fee_percentage), the global platform_fee and the
-- guest booking_fee are untouched.

-- The host guide's bands, versioned: an accepted Boost keeps the version it
-- was quoted on. min/max_rate_bps bound what an admin may propose.
CREATE TABLE IF NOT EXISTS boost_rate_bands (
    version          INTEGER NOT NULL,
    min_price_cents  BIGINT  NOT NULL,
    max_price_cents  BIGINT,              -- NULL = no upper bound
    guide_rate_bps   INTEGER NOT NULL,
    min_rate_bps     INTEGER NOT NULL,
    max_rate_bps     INTEGER NOT NULL,
    active_from      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (version, min_price_cents),
    CHECK (min_rate_bps <= guide_rate_bps AND guide_rate_bps <= max_rate_bps),
    CHECK (min_rate_bps >= 0 AND max_rate_bps <= 10000)
);
INSERT INTO boost_rate_bands (version, min_price_cents, max_price_cents, guide_rate_bps, min_rate_bps, max_rate_bps) VALUES
    (1,      0,  49999, 2000, 2000, 2000),   -- under ₹500: fixed 20%
    (1,  50000,  99999, 2000, 2000, 2000),   -- ₹500–999: fixed 20%
    (1, 100000, 199999, 1500, 1200, 2000),   -- ₹1,000–1,999: guide 15%
    (1, 200000, 499999, 1200, 1200, 2000),   -- ₹2,000–4,999: guide 12%
    (1, 500000,   NULL, 1200, 1200, 2000)    -- ₹5,000+: guide 12%
ON CONFLICT DO NOTHING;

-- Status names are the website's: under_review → quote_ready (accepted_at set
-- once the host accepts) → live → ended; declined (admin) and cancelled (host,
-- before live) are exits.
CREATE TABLE IF NOT EXISTS event_boosts (
    id                   UUID PRIMARY KEY,
    event_id             UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    host_id              UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    status               TEXT NOT NULL CHECK (status IN ('under_review','quote_ready','live','ended','declined','cancelled')),
    goal_text            TEXT,
    audience_notes       TEXT,
    channels             TEXT[] NOT NULL DEFAULT '{}',
    requested_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    proposed_at          TIMESTAMPTZ,
    admin_note           TEXT,
    decline_reason       TEXT,
    planned_from         TIMESTAMPTZ,
    planned_until        TIMESTAMPTZ,
    accepted_at          TIMESTAMPTZ,
    accepted_ip          TEXT,
    accepted_user_agent  TEXT,
    terms_version        TEXT,
    live_from            TIMESTAMPTZ,
    live_until           TIMESTAMPTZ,     -- set when it ends (or planned end while live)
    ended_reason         TEXT,
    reach                BIGINT NOT NULL DEFAULT 0,
    clicks               BIGINT NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One open Boost per event.
CREATE UNIQUE INDEX IF NOT EXISTS event_boosts_one_open_uq
    ON event_boosts (event_id) WHERE status IN ('under_review', 'quote_ready', 'live');
CREATE INDEX IF NOT EXISTS event_boosts_host_idx   ON event_boosts (host_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS event_boosts_status_idx ON event_boosts (status, requested_at DESC);

-- The proposed rate per ticket type; locked once accepted. ticket_key matches
-- a booking's ticket type: '' for the event's single price, else the lower-cased
-- ticket name (editing an event re-creates its ticket rows with new ids, and
-- bookings already match ticket types by name).
CREATE TABLE IF NOT EXISTS event_boost_rates (
    boost_id        UUID NOT NULL REFERENCES event_boosts(id) ON DELETE CASCADE,
    ticket_key      TEXT NOT NULL,
    price_tier_id   UUID REFERENCES event_price_tiers(id) ON DELETE SET NULL,  -- as quoted; informational
    ticket_name     TEXT NOT NULL,
    price_cents     BIGINT NOT NULL,
    guide_rate_bps  INTEGER NOT NULL,
    rate_bps        INTEGER NOT NULL CHECK (rate_bps BETWEEN 1200 AND 2000),
    reason          TEXT,
    band_version    INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS event_boost_rates_uq ON event_boost_rates (boost_id, ticket_key);

-- Audit trail: every request, proposal, acceptance, go-live and stop.
CREATE TABLE IF NOT EXISTS event_boost_log (
    id          BIGSERIAL PRIMARY KEY,
    boost_id    UUID NOT NULL REFERENCES event_boosts(id) ON DELETE CASCADE,
    actor_kind  TEXT NOT NULL CHECK (actor_kind IN ('host', 'admin', 'system')),
    actor       TEXT,
    action      TEXT NOT NULL,
    details     JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS event_boost_log_boost_idx ON event_boost_log (boost_id, created_at);

-- Commission stamped on a booking paid while a Boost was live (NULL / 0 otherwise).
-- boost_commission_gst_cents is the GST inside it (18/118), internal only.
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS boost_id                   UUID REFERENCES event_boosts(id);
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS boost_rate_bps             INTEGER;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS boost_commission_cents     BIGINT NOT NULL DEFAULT 0;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS boost_commission_gst_cents BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS bookings_boost_idx ON bookings (boost_id) WHERE boost_id IS NOT NULL;

-- Pilot: only invited hosts may request a Boost.
ALTER TABLE hosts ADD COLUMN IF NOT EXISTS boost_invited BOOLEAN NOT NULL DEFAULT false;

-- +migrate Down
ALTER TABLE hosts DROP COLUMN IF EXISTS boost_invited;
DROP INDEX IF EXISTS bookings_boost_idx;
ALTER TABLE bookings DROP COLUMN IF EXISTS boost_commission_gst_cents;
ALTER TABLE bookings DROP COLUMN IF EXISTS boost_commission_cents;
ALTER TABLE bookings DROP COLUMN IF EXISTS boost_rate_bps;
ALTER TABLE bookings DROP COLUMN IF EXISTS boost_id;
DROP TABLE IF EXISTS event_boost_log;
DROP TABLE IF EXISTS event_boost_rates;
DROP TABLE IF EXISTS event_boosts;
DROP TABLE IF EXISTS boost_rate_bands;
