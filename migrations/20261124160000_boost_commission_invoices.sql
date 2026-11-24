-- +migrate Up
-- Boost commission invoices (roadmap task 25): once a month, a GST tax invoice
-- to each host for the Event Boost commission on their kept boosted bookings,
-- one line per event; a credit note against it when an invoiced booking is
-- later refunded.

-- Settings: commission has its own series and SAC code (CA to confirm the code).
ALTER TABLE billing_settings ADD COLUMN IF NOT EXISTS commission_invoice_prefix TEXT NOT NULL DEFAULT 'M'
    CHECK (commission_invoice_prefix ~ '^[A-Z]{1,3}$');
ALTER TABLE billing_settings ADD COLUMN IF NOT EXISTS commission_sac_code TEXT NOT NULL DEFAULT '';

-- Invoices: a third kind of sale, the host's monthly commission. source_id is
-- the host; period the month billed ('2026-11'). Buyer GSTIN / state for B2B.
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_source_type_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_source_type_check
    CHECK (source_type IN ('booking', 'pass', 'host_commission'));
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS period           TEXT;
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS buyer_gstin      TEXT;
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS buyer_state_code TEXT;

-- One fee invoice and one fee credit note per sale (as before); one commission
-- invoice per host per month. Commission credit notes are tracked per booking
-- (bookings.boost_credit_note_id), so a host can have several.
DROP INDEX IF EXISTS invoices_source_kind_uq;
CREATE UNIQUE INDEX IF NOT EXISTS invoices_fee_source_kind_uq
    ON invoices (source_type, source_id, kind) WHERE source_type IN ('booking', 'pass');
CREATE UNIQUE INDEX IF NOT EXISTS invoices_commission_month_uq
    ON invoices (source_id, period) WHERE source_type = 'host_commission' AND kind = 'invoice';

-- Which commission invoice (and credit note) covers each boosted booking.
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS boost_invoice_id     UUID REFERENCES invoices(id);
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS boost_credit_note_id UUID REFERENCES invoices(id);
CREATE INDEX IF NOT EXISTS bookings_boost_uninvoiced_idx
    ON bookings (created_at) WHERE boost_commission_cents > 0 AND boost_invoice_id IS NULL;

-- Months the commission run has completed (the daily job runs each month once).
CREATE TABLE IF NOT EXISTS billing_runs (
    kind       TEXT NOT NULL,
    period     TEXT NOT NULL,
    ran_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    invoices   INTEGER NOT NULL DEFAULT 0,
    credit_notes INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (kind, period)
);

-- +migrate Down
DROP TABLE IF EXISTS billing_runs;
DROP INDEX IF EXISTS bookings_boost_uninvoiced_idx;
ALTER TABLE bookings DROP COLUMN IF EXISTS boost_credit_note_id;
ALTER TABLE bookings DROP COLUMN IF EXISTS boost_invoice_id;
DROP INDEX IF EXISTS invoices_commission_month_uq;
DROP INDEX IF EXISTS invoices_fee_source_kind_uq;
CREATE UNIQUE INDEX IF NOT EXISTS invoices_source_kind_uq ON invoices (source_type, source_id, kind);
ALTER TABLE invoices DROP COLUMN IF EXISTS buyer_state_code;
ALTER TABLE invoices DROP COLUMN IF EXISTS buyer_gstin;
ALTER TABLE invoices DROP COLUMN IF EXISTS period;
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_source_type_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_source_type_check CHECK (source_type IN ('booking', 'pass'));
ALTER TABLE billing_settings DROP COLUMN IF EXISTS commission_sac_code;
ALTER TABLE billing_settings DROP COLUMN IF EXISTS commission_invoice_prefix;
