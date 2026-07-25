-- +migrate Up
-- Adds a social profile link (Facebook/Instagram) to the reusable attendee
-- profile. Optional catalog field, enabled per-event like the others.
ALTER TABLE attendee_profiles ADD COLUMN IF NOT EXISTS social_link TEXT;

-- +migrate Down
ALTER TABLE attendee_profiles DROP COLUMN IF EXISTS social_link;
