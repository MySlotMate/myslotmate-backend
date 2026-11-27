-- +migrate Up
-- Premium Hosts and paid Boost marketing (PRD 2026-11-26). The host pays the
-- ad budget upfront (plus GST, not refundable) and a lower marketing fee
-- (band version 2, 4–12%) on tickets sold while live. Only Active Premium
-- hosts can Boost. Existing Boosts stay pricing_model 'no_upfront' on their
-- version 1 rates until they end.

-- Rates: version 2 goes down to 4%, so the 12–20% check has to go. The band
-- table's own min/max bound what an admin may propose.
ALTER TABLE event_boost_rates DROP CONSTRAINT IF EXISTS event_boost_rates_rate_bps_check;
ALTER TABLE event_boost_rates ADD CONSTRAINT event_boost_rates_rate_bps_check CHECK (rate_bps BETWEEN 0 AND 10000);

-- Band version 2: higher price, lower fee. Only budget Boosts are priced on
-- version 2 and later; no_upfront Boosts always use version 1.
INSERT INTO boost_rate_bands (version, min_price_cents, max_price_cents, guide_rate_bps, min_rate_bps, max_rate_bps) VALUES
    (2,      0,  49999, 1000,  800, 1200),   -- under ₹500: 10% (8–12%)
    (2,  50000,  99999,  800,  600, 1000),   -- ₹500–999: 8% (6–10%)
    (2, 100000, 199999,  700,  500, 1000),   -- ₹1,000–1,999: 7% (5–10%)
    (2, 200000, 499999,  600,  500,  800),   -- ₹2,000–4,999: 6% (5–8%)
    (2, 500000,   NULL,  500,  400,  800)    -- ₹5,000+: 5% (4–8%)
ON CONFLICT DO NOTHING;

-- The budget plan on a Boost. budget_from/until are IST calendar days, both
-- included. budget_total_cents is daily × days plus paid top-ups (before GST);
-- spent_cents the running total of the spend log.
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS pricing_model TEXT NOT NULL DEFAULT 'no_upfront'
    CHECK (pricing_model IN ('no_upfront', 'budget'));
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS daily_budget_cents BIGINT CHECK (daily_budget_cents > 0);
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS budget_from   DATE;
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS budget_until  DATE;
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS budget_total_cents BIGINT NOT NULL DEFAULT 0 CHECK (budget_total_cents >= 0);
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS budget_gst_cents   BIGINT NOT NULL DEFAULT 0 CHECK (budget_gst_cents >= 0);
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS budget_status TEXT NOT NULL DEFAULT 'not_required'
    CHECK (budget_status IN ('unpaid', 'paid', 'not_required'));
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS payment_due_by TIMESTAMPTZ;
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS spent_cents BIGINT NOT NULL DEFAULT 0 CHECK (spent_cents >= 0);
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;
ALTER TABLE event_boosts ADD COLUMN IF NOT EXISTS closed_by TEXT;
CREATE INDEX IF NOT EXISTS event_boosts_payment_due_idx
    ON event_boosts (payment_due_by) WHERE budget_status = 'unpaid' AND status = 'quote_ready';

-- One row per payment towards a Boost: the budget, then any top-ups. Amounts
-- are before GST; the host pays amount_cents + gst_cents.
CREATE TABLE IF NOT EXISTS boost_payments (
    id                     UUID PRIMARY KEY,
    boost_id               UUID NOT NULL REFERENCES event_boosts(id) ON DELETE CASCADE,
    kind                   TEXT NOT NULL CHECK (kind IN ('budget', 'top_up')),
    amount_cents           BIGINT NOT NULL CHECK (amount_cents > 0),
    gst_cents              BIGINT NOT NULL CHECK (gst_cents >= 0),
    days_added             INTEGER NOT NULL DEFAULT 0 CHECK (days_added >= 0),
    new_daily_budget_cents BIGINT,                 -- top-up that raises the daily budget
    method                 TEXT NOT NULL CHECK (method IN ('checkout', 'payment_link', 'offline')),
    razorpay_order_id      TEXT UNIQUE,
    razorpay_payment_id    TEXT UNIQUE,
    razorpay_link_id       TEXT UNIQUE,
    link_url               TEXT,
    offline_reference      TEXT,
    offline_paid_on        DATE,
    recorded_by            TEXT,
    status                 TEXT NOT NULL CHECK (status IN ('created', 'paid', 'failed', 'expired', 'reversed')),
    reverse_reason         TEXT,
    invoice_id             UUID REFERENCES invoices(id),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at                TIMESTAMPTZ,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS boost_payments_boost_idx ON boost_payments (boost_id, created_at);
CREATE INDEX IF NOT EXISTS boost_payments_uninvoiced_idx ON boost_payments (paid_at) WHERE status = 'paid' AND invoice_id IS NULL;

-- The team's daily spend log, per channel. Locked once the Boost is closed.
CREATE TABLE IF NOT EXISTS boost_spend_entries (
    boost_id     UUID NOT NULL REFERENCES event_boosts(id) ON DELETE CASCADE,
    day          DATE NOT NULL,
    channel      TEXT NOT NULL CHECK (channel IN ('instagram', 'facebook', 'google', 'whatsapp', 'influencers')),
    spend_cents  BIGINT NOT NULL DEFAULT 0 CHECK (spend_cents >= 0),
    impressions  BIGINT NOT NULL DEFAULT 0 CHECK (impressions >= 0),
    reach        BIGINT NOT NULL DEFAULT 0 CHECK (reach >= 0),
    clicks       BIGINT NOT NULL DEFAULT 0 CHECK (clicks >= 0),
    note         TEXT,
    entered_by   TEXT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (boost_id, day, channel)
);

-- Premium Hosts: one row per host; its history in premium_membership_log.
CREATE TABLE IF NOT EXISTS premium_memberships (
    host_id          UUID PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    status           TEXT NOT NULL CHECK (status IN ('applied', 'active', 'declined', 'removed')),
    applied_at       TIMESTAMPTZ,
    application_note TEXT,
    decided_at       TIMESTAMPTZ,
    decided_by       TEXT,
    granted          BOOLEAN NOT NULL DEFAULT false,
    manager_name     TEXT,
    manager_contact  TEXT,
    decline_reason   TEXT,
    remove_reason    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS premium_memberships_status_idx ON premium_memberships (status, updated_at DESC);

CREATE TABLE IF NOT EXISTS premium_membership_log (
    id          BIGSERIAL PRIMARY KEY,
    host_id     UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    actor_kind  TEXT NOT NULL CHECK (actor_kind IN ('host', 'admin', 'system')),
    actor       TEXT,
    action      TEXT NOT NULL,
    details     JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS premium_membership_log_host_idx ON premium_membership_log (host_id, created_at);

-- Launch day: today's pilot hosts become Premium (a grant), so Boost keeps
-- working for them.
INSERT INTO premium_memberships (host_id, status, decided_at, decided_by, granted)
SELECT id, 'active', now(), 'migration', true FROM hosts WHERE boost_invited
ON CONFLICT (host_id) DO NOTHING;
INSERT INTO premium_membership_log (host_id, actor_kind, actor, action, details)
SELECT id, 'system', 'migration', 'granted', '{"from":"boost_invited"}'::jsonb FROM hosts WHERE boost_invited;

-- The creatives space: what runs in the ads. files is an ordered JSON list of
-- {key, name, mime_type, size}.
CREATE TABLE IF NOT EXISTS boost_creatives (
    id               UUID PRIMARY KEY,
    boost_id         UUID NOT NULL REFERENCES event_boosts(id) ON DELETE CASCADE,
    type             TEXT NOT NULL CHECK (type IN ('reel', 'post', 'story', 'brand')),
    files            JSONB NOT NULL DEFAULT '[]',
    caption          TEXT,
    channels         TEXT[] NOT NULL DEFAULT '{}',
    uploaded_by_kind TEXT NOT NULL CHECK (uploaded_by_kind IN ('host', 'admin')),
    uploaded_by      TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('submitted', 'in_use', 'needs_changes', 'not_used',
                                                     'awaiting_approval', 'approved', 'changes_requested')),
    status_note      TEXT,
    replaces_id      UUID REFERENCES boost_creatives(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS boost_creatives_boost_idx ON boost_creatives (boost_id, created_at);

-- Reports the team shares. A replaced report keeps its row (replaced_at set)
-- for the history and is no longer shown.
CREATE TABLE IF NOT EXISTS boost_reports (
    id           UUID PRIMARY KEY,
    boost_id     UUID NOT NULL REFERENCES event_boosts(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('update', 'final')),
    title        TEXT NOT NULL,
    covers_from  DATE,
    covers_until DATE,
    note         TEXT,
    file_key     TEXT NOT NULL,
    file_name    TEXT NOT NULL,
    mime_type    TEXT NOT NULL,
    size         BIGINT NOT NULL,
    uploaded_by  TEXT NOT NULL,
    replaces_id  UUID REFERENCES boost_reports(id) ON DELETE SET NULL,
    replaced_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS boost_reports_boost_idx ON boost_reports (boost_id, created_at);

-- Budget rules the team can change from the admin (one row).
-- budget_required: once true, new requests must carry a budget plan (the
-- launch switch); until then a request without one is a no_upfront Boost.
CREATE TABLE IF NOT EXISTS boost_settings (
    id                     BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    min_daily_budget_cents BIGINT  NOT NULL DEFAULT 50000 CHECK (min_daily_budget_cents > 0),
    budget_step_cents      BIGINT  NOT NULL DEFAULT 10000 CHECK (budget_step_cents > 0),
    min_days               INTEGER NOT NULL DEFAULT 3 CHECK (min_days > 0),
    payment_window_days    INTEGER NOT NULL DEFAULT 7 CHECK (payment_window_days > 0),
    budget_gst_bps         INTEGER NOT NULL DEFAULT 1800 CHECK (budget_gst_bps BETWEEN 0 AND 2800),
    budget_required        BOOLEAN NOT NULL DEFAULT false,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO boost_settings (id) VALUES (true) ON CONFLICT DO NOTHING;

-- Billing: the budget invoice, a fourth kind of document, with its own
-- series and SAC code (CA to confirm). source_id is the boost payment.
ALTER TABLE billing_settings ADD COLUMN IF NOT EXISTS budget_invoice_prefix TEXT NOT NULL DEFAULT 'B'
    CHECK (budget_invoice_prefix ~ '^[A-Z]{1,3}$');
ALTER TABLE billing_settings ADD COLUMN IF NOT EXISTS budget_sac_code TEXT NOT NULL DEFAULT '';
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_source_type_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_source_type_check
    CHECK (source_type IN ('booking', 'pass', 'host_commission', 'boost_budget'));
CREATE UNIQUE INDEX IF NOT EXISTS invoices_budget_payment_uq
    ON invoices (source_id, kind) WHERE source_type = 'boost_budget';

-- +migrate Down
DROP INDEX IF EXISTS invoices_budget_payment_uq;
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_source_type_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_source_type_check
    CHECK (source_type IN ('booking', 'pass', 'host_commission'));
ALTER TABLE billing_settings DROP COLUMN IF EXISTS budget_sac_code;
ALTER TABLE billing_settings DROP COLUMN IF EXISTS budget_invoice_prefix;
DROP TABLE IF EXISTS boost_settings;
DROP TABLE IF EXISTS boost_reports;
DROP TABLE IF EXISTS boost_creatives;
DROP TABLE IF EXISTS premium_membership_log;
DROP TABLE IF EXISTS premium_memberships;
DROP TABLE IF EXISTS boost_spend_entries;
DROP TABLE IF EXISTS boost_payments;
DROP INDEX IF EXISTS event_boosts_payment_due_idx;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS closed_by;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS closed_at;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS spent_cents;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS payment_due_by;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS budget_status;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS budget_gst_cents;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS budget_total_cents;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS budget_until;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS budget_from;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS daily_budget_cents;
ALTER TABLE event_boosts DROP COLUMN IF EXISTS pricing_model;
DELETE FROM boost_rate_bands WHERE version >= 2;
ALTER TABLE event_boost_rates DROP CONSTRAINT IF EXISTS event_boost_rates_rate_bps_check;
ALTER TABLE event_boost_rates ADD CONSTRAINT event_boost_rates_rate_bps_check CHECK (rate_bps BETWEEN 1200 AND 2000);
