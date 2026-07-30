-- +migrate Up
-- Split codes into two independent kinds via grants_free:
--   grants_free = FALSE → an ACCESS code (a per-guest passkey): it unlocks a
--                         private event but the guest pays the normal price.
--   grants_free = TRUE  → a FREE-BOOKING code (comp): it waives payment to ₹0
--                         (and also unlocks a private event).
-- Access (passkey) and free (payment) are now decoupled: a passkey never implies
-- free — "free for everyone with the passkey" is expressed by pricing the whole
-- event as free, and events.passkey_grants_free is no longer consulted.
ALTER TABLE coupons ADD COLUMN IF NOT EXISTS grants_free BOOLEAN NOT NULL DEFAULT TRUE;

-- +migrate Down
ALTER TABLE coupons DROP COLUMN IF EXISTS grants_free;
