-- +migrate Up
-- Last time guests were told the host shared photos from this event; caps the
-- "{Host} shared {n} photos" message at one per event per day.
ALTER TABLE events ADD COLUMN IF NOT EXISTS gallery_notified_at TIMESTAMPTZ;

-- +migrate Down
ALTER TABLE events DROP COLUMN IF EXISTS gallery_notified_at;
