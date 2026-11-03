-- +migrate Up
-- A third code kind next to access (grants_free = FALSE) and free (grants_free = TRUE):
-- a PERCENTAGE-DISCOUNT code takes discount_percent off the ticket total. It is
-- never a free code too — a 100% discount is what grants_free already means.
ALTER TABLE coupons ADD COLUMN IF NOT EXISTS discount_percent INT;
ALTER TABLE coupons DROP CONSTRAINT IF EXISTS coupons_discount_percent_check;
ALTER TABLE coupons ADD CONSTRAINT coupons_discount_percent_check
    CHECK (discount_percent IS NULL OR (discount_percent BETWEEN 1 AND 99 AND NOT grants_free));

-- +migrate Down
ALTER TABLE coupons DROP CONSTRAINT IF EXISTS coupons_discount_percent_check;
ALTER TABLE coupons DROP COLUMN IF EXISTS discount_percent;
