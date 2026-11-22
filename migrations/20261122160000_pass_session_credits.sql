-- +migrate Up
-- Monthly pass becomes a balance of sessions ("top-up") instead of a set of
-- bookings made at purchase. The guest picks each session and pays for it with
-- one session off the pass. Money still moves once, at purchase; nothing about
-- user_passes' money columns changes.
--
-- expiry_reminded_at: the one "your pass ends soon, N sessions left" message is
-- claimed by setting this, so it is never sent twice.
ALTER TABLE user_passes ADD COLUMN IF NOT EXISTS expiry_reminded_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_user_passes_expiry_reminder
    ON user_passes (valid_until) WHERE status = 'active' AND expiry_reminded_at IS NULL;

-- +migrate Down
DROP INDEX IF EXISTS idx_user_passes_expiry_reminder;
ALTER TABLE user_passes DROP COLUMN IF EXISTS expiry_reminded_at;
