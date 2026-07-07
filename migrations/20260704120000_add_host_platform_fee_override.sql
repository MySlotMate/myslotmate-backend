-- +migrate Up
-- Per-host commission override. NULL means "use the global platform_settings
-- 'platform_fee' default (currently 70/30)". When set, this is the platform's
-- cut for that host's bookings — e.g. 20 means host keeps 80%, platform 20%.
ALTER TABLE hosts ADD COLUMN platform_fee_percentage SMALLINT NULL
	CHECK (platform_fee_percentage IS NULL OR (platform_fee_percentage >= 0 AND platform_fee_percentage <= 100));

-- +migrate Down
ALTER TABLE hosts DROP COLUMN IF EXISTS platform_fee_percentage;
