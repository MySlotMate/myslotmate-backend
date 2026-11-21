-- +migrate Up
-- Marketing Boost v2, Phase 1: statutory tax deductions on host payouts.
-- GST TCS (Section 52 CGST Act) and income-tax TDS (Section 194-O) are stamped
-- on each booking at confirmation; rates live in platform_settings.tax_settings.

-- Host tax profile, collected at first withdrawal (not at signup).
CREATE TABLE IF NOT EXISTS host_tax_profiles (
    host_id        UUID PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    entity_type    TEXT NOT NULL CHECK (entity_type IN ('individual','huf','company','firm','llp','other')),
    pan            TEXT NOT NULL CHECK (pan ~ '^[A-Z]{5}[0-9]{4}[A-Z]$'),
    pan_name       TEXT NOT NULL CHECK (length(trim(pan_name)) > 0),
    gstin          TEXT NULL CHECK (gstin IS NULL OR (gstin ~ '^[0-9]{2}[A-Z0-9]{10}[0-9A-Z]{3}$' AND substr(gstin, 3, 10) = pan)),
    -- First two digits of the GSTIN; derived server-side, never taken from the client.
    gst_state_code CHAR(2) NULL CHECK (gst_state_code IS NULL OR gst_state_code = substr(gstin, 1, 2)),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-booking tax lines (integer paise). ticket_cents is the ticket value the
-- taxes are based on: unit price x quantity after coupon discount, excluding the
-- guest booking fee. host_payout_cents NULL = booked before tax stamping existed
-- (readers fall back to net_earning_cents).
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS ticket_cents           BIGINT;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS platform_fee_gst_cents BIGINT  NOT NULL DEFAULT 0;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS tcs_cents              BIGINT  NOT NULL DEFAULT 0;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS tcs_interstate         BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS tds_cents              BIGINT  NOT NULL DEFAULT 0;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS host_payout_cents      BIGINT;

-- Backfill: the guest booking fee has been 0 in production, so the ticket value
-- of every existing booking is its amount.
UPDATE bookings SET ticket_cents = amount_cents
 WHERE ticket_cents IS NULL AND amount_cents IS NOT NULL;

-- GST state code of the venue, for the CGST/SGST vs IGST split.
ALTER TABLE events ADD COLUMN IF NOT EXISTS state_code CHAR(2)
    CHECK (state_code IS NULL OR state_code ~ '^[0-9]{2}$');

-- TCS/TDS the platform has deposited with the government, logged by finance.
-- Undeposited tax (stamped minus deposited) is held out of platform withdrawals.
CREATE TABLE IF NOT EXISTS tax_deposits (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         TEXT NOT NULL CHECK (kind IN ('tcs','tds')),
    period       TEXT NOT NULL CHECK (period ~ '^[0-9]{4}-[0-9]{2}$'),
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    challan_ref  TEXT,
    deposited_at TIMESTAMPTZ NOT NULL,
    created_by   UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Supports the per-host financial-year gross sum taken inside the booking tx.
CREATE INDEX IF NOT EXISTS idx_bookings_event_created ON bookings (event_id, created_at);

-- Ships switched off: stamping and the PAN gate are flipped from admin Settings.
INSERT INTO platform_settings (key, value) VALUES
  ('tax_settings', '{"enabled": false, "require_pan_for_withdrawal": false, "tcs_bps": 50, "tds_bps": 10, "tds_threshold_cents": 50000000}')
ON CONFLICT (key) DO NOTHING;

-- +migrate Down
DELETE FROM platform_settings WHERE key = 'tax_settings';
DROP INDEX IF EXISTS idx_bookings_event_created;
DROP TABLE IF EXISTS tax_deposits;
ALTER TABLE events DROP COLUMN IF EXISTS state_code;
ALTER TABLE bookings DROP COLUMN IF EXISTS host_payout_cents;
ALTER TABLE bookings DROP COLUMN IF EXISTS tds_cents;
ALTER TABLE bookings DROP COLUMN IF EXISTS tcs_interstate;
ALTER TABLE bookings DROP COLUMN IF EXISTS tcs_cents;
ALTER TABLE bookings DROP COLUMN IF EXISTS platform_fee_gst_cents;
ALTER TABLE bookings DROP COLUMN IF EXISTS ticket_cents;
DROP TABLE IF EXISTS host_tax_profiles;
