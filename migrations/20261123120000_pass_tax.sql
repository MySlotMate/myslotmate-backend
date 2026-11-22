-- +migrate Up
-- Marketing Boost v2: statutory tax on monthly pass sales. A pass is a ticket
-- sale like any booking, so it carries the same tax lines (integer paise) as
-- bookings (see 20261122120000_host_tax_profile.sql). ticket_cents is the pass
-- price excluding the guest booking fee. host_payout_cents NULL = sold before
-- tax stamping existed (readers fall back to net_earning_cents).
ALTER TABLE user_passes ADD COLUMN IF NOT EXISTS ticket_cents           BIGINT;
ALTER TABLE user_passes ADD COLUMN IF NOT EXISTS platform_fee_gst_cents BIGINT  NOT NULL DEFAULT 0;
ALTER TABLE user_passes ADD COLUMN IF NOT EXISTS tcs_cents              BIGINT  NOT NULL DEFAULT 0;
ALTER TABLE user_passes ADD COLUMN IF NOT EXISTS tcs_interstate         BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE user_passes ADD COLUMN IF NOT EXISTS tds_cents              BIGINT  NOT NULL DEFAULT 0;
ALTER TABLE user_passes ADD COLUMN IF NOT EXISTS host_payout_cents      BIGINT;

-- Backfill: the guest booking fee has been 0 in production, so the ticket value
-- of every existing pass is its price.
UPDATE user_passes SET ticket_cents = price_cents WHERE ticket_cents IS NULL;

-- +migrate Down
ALTER TABLE user_passes DROP COLUMN IF EXISTS host_payout_cents;
ALTER TABLE user_passes DROP COLUMN IF EXISTS tds_cents;
ALTER TABLE user_passes DROP COLUMN IF EXISTS tcs_interstate;
ALTER TABLE user_passes DROP COLUMN IF EXISTS tcs_cents;
ALTER TABLE user_passes DROP COLUMN IF EXISTS platform_fee_gst_cents;
ALTER TABLE user_passes DROP COLUMN IF EXISTS ticket_cents;
