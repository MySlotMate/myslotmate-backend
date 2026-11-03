-- +migrate Up
-- Admin-selected hosts get their full withdrawable balance paid out
-- automatically every Tuesday (IST) to a verified bank account. Hosts without
-- a verified bank payout method are skipped. See payoutService.RunAutoPayouts.
ALTER TABLE hosts ADD COLUMN auto_payout_enabled BOOLEAN NOT NULL DEFAULT false;

-- +migrate Down
ALTER TABLE hosts DROP COLUMN IF EXISTS auto_payout_enabled;
