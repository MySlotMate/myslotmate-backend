-- +migrate Up
-- Source-refund support: a refund-type payment row can now point at the
-- original top-up (`refund_of_payment_id`) and carry the Razorpay refund id
-- (`gateway_refund_id`). Used by the admin-gated "refund to source" flow that
-- sends money back to the customer's card via the Razorpay Refunds API.

ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS gateway_refund_id   TEXT,                       -- Razorpay rfnd_xxxxx
    ADD COLUMN IF NOT EXISTS refund_of_payment_id UUID REFERENCES payments(id);

-- One payments row per Razorpay refund id.
CREATE UNIQUE INDEX IF NOT EXISTS idx_payments_gateway_refund_id
    ON payments (gateway_refund_id)
    WHERE gateway_refund_id IS NOT NULL;

-- Lookup: "all refunds against this top-up" (for computing refundable headroom).
CREATE INDEX IF NOT EXISTS idx_payments_refund_of_payment_id
    ON payments (refund_of_payment_id)
    WHERE refund_of_payment_id IS NOT NULL;

-- +migrate Down
DROP INDEX IF EXISTS idx_payments_refund_of_payment_id;
DROP INDEX IF EXISTS idx_payments_gateway_refund_id;
ALTER TABLE payments
    DROP COLUMN IF EXISTS refund_of_payment_id,
    DROP COLUMN IF EXISTS gateway_refund_id;
