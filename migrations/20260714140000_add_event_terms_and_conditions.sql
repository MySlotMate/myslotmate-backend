-- +migrate Up
-- Per-experience terms & conditions, set by the admin on the experience form and
-- printed on the guest's ticket PDF. Nullable — events without terms print none.

ALTER TABLE events ADD COLUMN IF NOT EXISTS terms_and_conditions TEXT;

-- +migrate Down
ALTER TABLE events DROP COLUMN IF EXISTS terms_and_conditions;
