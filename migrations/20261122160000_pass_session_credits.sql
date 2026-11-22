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

-- One live pass booking per pass per session. A booking cancelled with a refund
-- (status 'cancelled', session back on the pass) frees the slot so the guest can
-- book that session again; a late cancel keeps status 'confirmed' and holds it,
-- so a session given up late can't be rebooked with the pass.
CREATE UNIQUE INDEX IF NOT EXISTS idx_bookings_pass_occurrence_live
    ON bookings (pass_id, occurrence_date)
    WHERE pass_id IS NOT NULL AND status IN ('pending', 'confirmed');

-- +migrate Down
DROP INDEX IF EXISTS idx_bookings_pass_occurrence_live;
DROP INDEX IF EXISTS idx_user_passes_expiry_reminder;
ALTER TABLE user_passes DROP COLUMN IF EXISTS expiry_reminded_at;
