-- +migrate Up
-- Soft-delete / deactivate flag for hosts. Admin can deactivate a host to hide
-- them (and their events) from the public site while preserving all bookings,
-- earnings and history. Reversible. Never hard-delete a host — that cascades
-- away events/bookings/earnings/wallet.

ALTER TABLE hosts ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT TRUE;

-- +migrate Down
ALTER TABLE hosts DROP COLUMN IF EXISTS is_active;
