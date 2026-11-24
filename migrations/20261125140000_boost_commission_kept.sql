-- +migrate Up
-- Event Boost refund rule 1: when the HOST refunds a boosted booking (cancels
-- the event, pauses its sessions), the guest is refunded in full but the Boost
-- commission on that booking stays owed by the host: the ads already ran. This
-- flag marks those bookings so earnings, the monthly commission invoice, the
-- host statement and the Boost's totals keep counting the commission.
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS boost_commission_kept BOOLEAN NOT NULL DEFAULT false;

-- +migrate Down
ALTER TABLE bookings DROP COLUMN IF EXISTS boost_commission_kept;
