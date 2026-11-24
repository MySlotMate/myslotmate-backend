-- +migrate Up
-- The host editor offers a "No Refund" policy and both backends treat
-- 'no_refund' as non-refundable, but the enum only had flexible / moderate /
-- strict, so saving an event with No Refund failed. Event Boost now asks for
-- Moderate, Strict or No refunds, so the value has to exist.
-- ALTER TYPE ... ADD VALUE runs in autocommit (same as add_processing_status).
ALTER TYPE cancellation_policy ADD VALUE IF NOT EXISTS 'no_refund';

-- +migrate Down
-- Postgres cannot drop an enum value; nothing to undo.
