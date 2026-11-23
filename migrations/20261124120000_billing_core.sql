-- +migrate Up
-- Billing core (roadmap task 18): a GST tax invoice for the guest booking fee
-- on every paid booking and monthly pass, and a credit note when it is refunded.
--
-- Money is integer paise. An invoice row keeps a copy of everything printed on
-- it (supplier, buyer, amounts), so its PDF can be rebuilt exactly at any time
-- even after the settings or the user's profile change.

-- One row: MySlotMate's own details as the supplier. Invoices are issued only
-- while enabled and every required field is filled in.
CREATE TABLE IF NOT EXISTS billing_settings (
    id                  BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    enabled             BOOLEAN NOT NULL DEFAULT false,
    legal_name          TEXT    NOT NULL DEFAULT '',
    trade_name          TEXT    NOT NULL DEFAULT '',
    gstin               TEXT    NOT NULL DEFAULT '',
    pan                 TEXT    NOT NULL DEFAULT '',
    address             TEXT    NOT NULL DEFAULT '',
    state_code          TEXT    NOT NULL DEFAULT '',
    email               TEXT    NOT NULL DEFAULT '',
    sac_code            TEXT    NOT NULL DEFAULT '',
    gst_rate_bps        INTEGER NOT NULL DEFAULT 1800 CHECK (gst_rate_bps BETWEEN 0 AND 2800),
    invoice_prefix      TEXT    NOT NULL DEFAULT 'F' CHECK (invoice_prefix ~ '^[A-Z]{1,3}$'),
    credit_note_prefix  TEXT    NOT NULL DEFAULT 'C' CHECK (credit_note_prefix ~ '^[A-Z]{1,3}$'),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO billing_settings (id) VALUES (true) ON CONFLICT DO NOTHING;

-- The last number used per prefix and financial year ("2627" = FY 2026-27).
-- Taken with an upsert inside the sale's transaction, so a rolled-back sale
-- gives its number back and the series has no gaps.
CREATE TABLE IF NOT EXISTS invoice_series (
    prefix      TEXT    NOT NULL,
    fy          TEXT    NOT NULL,
    last_number BIGINT  NOT NULL,
    PRIMARY KEY (prefix, fy)
);

CREATE TABLE IF NOT EXISTS invoices (
    id                   UUID PRIMARY KEY,
    kind                 TEXT NOT NULL CHECK (kind IN ('invoice', 'credit_note')),
    number               TEXT NOT NULL UNIQUE,
    fy                   TEXT NOT NULL,
    issued_at            TIMESTAMPTZ NOT NULL,
    -- What was sold: a booking or a monthly pass.
    source_type          TEXT NOT NULL CHECK (source_type IN ('booking', 'pass')),
    source_id            UUID NOT NULL,
    event_id             UUID,
    event_title          TEXT NOT NULL DEFAULT '',
    -- A credit note points at the invoice it reverses.
    original_invoice_id  UUID REFERENCES invoices(id),
    -- Buyer, as on the day of issue.
    user_id              UUID NOT NULL REFERENCES users(id),
    buyer_name           TEXT NOT NULL DEFAULT '',
    buyer_email          TEXT NOT NULL DEFAULT '',
    buyer_phone          TEXT NOT NULL DEFAULT '',
    place_of_supply      TEXT NOT NULL,          -- GST state code
    -- Supplier, as on the day of issue.
    supplier             JSONB NOT NULL,
    -- Totals; the lines add up to these.
    taxable_cents        BIGINT NOT NULL,
    cgst_cents           BIGINT NOT NULL DEFAULT 0,
    sgst_cents           BIGINT NOT NULL DEFAULT 0,
    igst_cents           BIGINT NOT NULL DEFAULT 0,
    total_cents          BIGINT NOT NULL,
    -- Archived PDF in S3 (private). NULL = not archived yet; it is rebuilt from
    -- this row on download either way.
    pdf_key              TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (total_cents = taxable_cents + cgst_cents + sgst_cents + igst_cents),
    CHECK ((kind = 'credit_note') = (original_invoice_id IS NOT NULL))
);
-- One invoice and at most one credit note per sale.
CREATE UNIQUE INDEX IF NOT EXISTS invoices_source_kind_uq ON invoices (source_type, source_id, kind);
CREATE INDEX IF NOT EXISTS invoices_user_idx   ON invoices (user_id, issued_at DESC);
CREATE INDEX IF NOT EXISTS invoices_issued_idx ON invoices (issued_at DESC);
CREATE INDEX IF NOT EXISTS invoices_unarchived_idx ON invoices (created_at) WHERE pdf_key IS NULL;

CREATE TABLE IF NOT EXISTS invoice_lines (
    id             UUID PRIMARY KEY,
    invoice_id     UUID NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    position       INTEGER NOT NULL,
    description    TEXT NOT NULL,
    sac_code       TEXT NOT NULL,
    quantity       INTEGER NOT NULL DEFAULT 1,
    taxable_cents  BIGINT NOT NULL,
    gst_rate_bps   INTEGER NOT NULL,
    cgst_cents     BIGINT NOT NULL DEFAULT 0,
    sgst_cents     BIGINT NOT NULL DEFAULT 0,
    igst_cents     BIGINT NOT NULL DEFAULT 0,
    total_cents    BIGINT NOT NULL,
    UNIQUE (invoice_id, position)
);

-- +migrate Down
DROP TABLE IF EXISTS invoice_lines;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS invoice_series;
DROP TABLE IF EXISTS billing_settings;
