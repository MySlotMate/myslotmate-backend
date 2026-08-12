-- +migrate Up
-- Bulk import for PAID events, with the offline-payment trail needed to track it.
--
-- The money rule this encodes: bulk import never moves money through the
-- platform. For a paid event the host collects the fee themselves (cash/UPI,
-- outside MySlotMate) and the booking is written at amount_cents = 0 — no wallet
-- debit, no transaction_ledger rows, no host_earnings credit, no payout
-- entitlement. Crediting host earnings here would invent money the platform
-- never received and would eventually be paid out for real.
--
-- What IS recorded, so the value is not lost:
--   bookings.unit_price_cents  — already exists; the per-seat price snapshot.
--                                amount_cents = 0 while unit_price_cents > 0 is
--                                precisely the signature of a paid-offline seat.
--   bookings.source            — how the booking was made.
--   bookings.import_job_id     — which upload produced it.

-- How a booking came to exist. 'online' = the normal guest checkout (the only
-- source that existed before this column, hence the default and the backfill).
-- NOTE: 'walk_in' does NOT imply "no money moved" — the on-spot paid path runs a
-- real wallet debit. Only source='bulk_import' together with the job's
-- payment_mode='offline' means the platform saw no money.
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'online';

-- Which bulk upload created this booking (NULL for every other source).
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS import_job_id UUID REFERENCES booking_import_jobs(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_bookings_source ON bookings (source) WHERE source <> 'online';
CREATE INDEX IF NOT EXISTS idx_bookings_import_job ON bookings (import_job_id) WHERE import_job_id IS NOT NULL;

-- How the job's bookings were paid for. Derived server-side from the event and
-- coupon at upload time — never accepted from the client, or it could disagree
-- with what actually happened:
--   'free'    — the event costs nothing.
--   'coupon'  — paid event comped to zero by a host coupon (code recorded above).
--   'offline' — paid event; the host collected the money outside the platform
--               and accepted responsibility for it (offline_ack).
ALTER TABLE booking_import_jobs ADD COLUMN IF NOT EXISTS payment_mode TEXT NOT NULL DEFAULT 'free';

-- Per-seat price at upload time, so the amount the host owed/collected offline
-- is reconstructable later even if the event is repriced or deleted.
ALTER TABLE booking_import_jobs ADD COLUMN IF NOT EXISTS unit_price_cents BIGINT NOT NULL DEFAULT 0;

-- The host's explicit "I have collected payment for these guests myself"
-- acknowledgment. Enforced server-side, not just in the UI — a checkbox that
-- only exists in React is not a record that anyone accepted responsibility.
ALTER TABLE booking_import_jobs ADD COLUMN IF NOT EXISTS offline_ack BOOLEAN NOT NULL DEFAULT FALSE;

-- +migrate Down
ALTER TABLE booking_import_jobs DROP COLUMN IF EXISTS offline_ack;
ALTER TABLE booking_import_jobs DROP COLUMN IF EXISTS unit_price_cents;
ALTER TABLE booking_import_jobs DROP COLUMN IF EXISTS payment_mode;
DROP INDEX IF EXISTS idx_bookings_import_job;
DROP INDEX IF EXISTS idx_bookings_source;
ALTER TABLE bookings DROP COLUMN IF EXISTS import_job_id;
ALTER TABLE bookings DROP COLUMN IF EXISTS source;
