-- +migrate Up
-- Marks that a host's avatar_url was pulled from their Instagram profile (by the
-- one-time scrape). Drives the small Instagram badge shown on the profile photo.
-- Reset to false when the host uploads their own photo.
ALTER TABLE hosts ADD COLUMN avatar_from_instagram BOOLEAN NOT NULL DEFAULT false;

-- +migrate Down
ALTER TABLE hosts DROP COLUMN IF EXISTS avatar_from_instagram;
