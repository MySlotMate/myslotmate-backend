-- +migrate Up
-- Add 'cancelled' to the event_status enum so eventService.CancelEvent
-- (host-initiated soft cancel that refunds all upcoming bookings via F4)
-- can mark the event row without failing on an enum check.

ALTER TYPE event_status ADD VALUE IF NOT EXISTS 'cancelled';

-- +migrate Down
-- PostgreSQL does not support removing values from an enum. The value will
-- remain even if this migration is rolled back — that is safe because nothing
-- depends on its absence.
